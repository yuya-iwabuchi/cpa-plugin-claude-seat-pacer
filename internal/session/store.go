package session

import (
	"container/list"
	"sort"
	"sync"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// Store holds the conversation-to-credential bindings.
//
// Every method is safe for concurrent use, because the pick path runs with no
// serialization from the host. There is no background goroutine: expiry is
// driven by the caller through Sweep, and by Lookup dropping the entry it
// happens to touch. A goroutine here would outlive nothing the host can
// cancel and would block dlclose.
//
// A binding is scoped per provider and model as well as per conversation,
// because a model can be served by a different credential set than its
// siblings: a provider-blind or model-blind lookup hands back a credential
// that cannot serve the request.
//
// The lock is an RWMutex because every open status page reads this table on
// each refresh while the pick path shares it: All and CountByAuth take it for
// reading only, so a status request cannot serialize routing. counts is maintained on every
// insert and removal for the same reason — the per-credential tally is read on
// every status render and on every pick that falls back to least-bound, and
// walking the whole table for it costs the cap, which is 65536 by default.
type Store struct {
	mu  sync.RWMutex
	ttl time.Duration
	max int
	// order is the access order, most recently seen at the front, which is
	// what eviction reads. index resolves a composed key in O(1).
	order *list.List
	index map[string]*list.Element
	// counts is bindings per credential id, sized by the pool rather than by
	// the table.
	counts map[string]int
}

// entry is one binding plus the composed key it is filed under, so removing it
// does not have to recompose that key from its parts.
type entry struct {
	index   string
	binding model.Binding
}

// NewStore returns an empty store. ttl bounds idle time rather than total
// session length, so reuse refreshes it; maxSessions caps the table, evicting
// the least recently seen. A non-positive ttl disables expiry and a
// non-positive maxSessions disables the cap, neither of which
// model.Config.Normalize permits.
func NewStore(ttl time.Duration, maxSessions int) *Store {
	return &Store{
		ttl:    ttl,
		max:    maxSessions,
		order:  list.New(),
		index:  make(map[string]*list.Element),
		counts: make(map[string]int),
	}
}

// indexKey composes the table key for one conversation on one provider and
// model. The session key is already opaque and bounded, and provider and model
// are host-supplied identifiers, so the composite needs no hashing.
func indexKey(provider, modelID, sessionKey string) string {
	return provider + "|" + modelID + "|" + sessionKey
}

// Lookup returns the binding for a conversation on one provider and model. A
// hit refreshes LastSeen and counts a hit; a binding idle past the TTL is a
// miss and is dropped. An empty session key names no conversation and always
// misses.
func (s *Store) Lookup(provider, modelID, sessionKey string, now time.Time) (model.Binding, bool) {
	if sessionKey == "" {
		return model.Binding{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.index[indexKey(provider, modelID, sessionKey)]
	if !ok {
		return model.Binding{}, false
	}
	e := el.Value.(*entry)
	if s.expired(e, now) {
		s.remove(el)
		return model.Binding{}, false
	}
	e.binding.LastSeen = now
	e.binding.Hits++
	s.order.MoveToFront(el)
	return e.binding, true
}

// Bind pins a conversation on one provider and model to a credential and
// returns the resulting binding. Rebinding to the same credential refreshes
// the existing binding; a different credential replaces it, because a new
// credential means a new cached prefix. An entry idle past the TTL is treated
// as absent, so its hit count and bind time do not carry into a new
// conversation that reuses the key. An empty session key names no conversation
// and binds nothing.
func (s *Store) Bind(provider, modelID, sessionKey, authID string, now time.Time) model.Binding {
	if sessionKey == "" {
		return model.Binding{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := indexKey(provider, modelID, sessionKey)
	if el, ok := s.index[key]; ok {
		e := el.Value.(*entry)
		if !s.expired(e, now) && e.binding.AuthID == authID {
			e.binding.LastSeen = now
			s.order.MoveToFront(el)
			return e.binding
		}
		s.remove(el)
	}

	e := &entry{
		index: key,
		binding: model.Binding{
			SessionKey: sessionKey,
			Provider:   provider,
			Model:      modelID,
			AuthID:     authID,
			BoundAt:    now,
			LastSeen:   now,
		},
	}
	s.index[key] = s.order.PushFront(e)
	s.counts[authID]++
	s.evict()
	return e.binding
}

// DropAuth unbinds every session on a credential and returns how many it
// removed. A credential that has gone away or been disabled must not keep
// sessions pinned to it.
func (s *Store) DropAuth(authID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for el := s.order.Front(); el != nil; {
		next := el.Next()
		if el.Value.(*entry).binding.AuthID == authID {
			s.remove(el)
			n++
		}
		el = next
	}
	return n
}

// All returns every binding, newest LastSeen first and ties broken by session
// key, provider and model, so the status UI renders the same table twice for
// the same state.
func (s *Store) All() []model.Binding {
	s.mu.RLock()
	out := make([]model.Binding, 0, len(s.index))
	for _, el := range s.index {
		out = append(out, el.Value.(*entry).binding)
	}
	s.mu.RUnlock()

	// Provider, model and session key together are the table key, so this
	// ordering is total and does not depend on how the entries were reached,
	// which is what lets the sort run outside the lock.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case !a.LastSeen.Equal(b.LastSeen):
			return a.LastSeen.After(b.LastSeen)
		case a.SessionKey != b.SessionKey:
			return a.SessionKey < b.SessionKey
		case a.Provider != b.Provider:
			return a.Provider < b.Provider
		default:
			return a.Model < b.Model
		}
	})
	return out
}

// CountByAuth reports how many live bindings each credential holds at now,
// which is the per-credential session count the status UI shows. A binding
// idle past the TTL is not counted, though it stays in the table until Sweep
// or Lookup removes it.
//
// Expired bindings sit at the back of the access order, so only they are
// walked: the cost is the tally plus the expired tail, never the table.
func (s *Store) CountByAuth(now time.Time) map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := make(map[string]int, len(s.counts))
	for authID, n := range s.counts {
		counts[authID] = n
	}
	for el := s.order.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*entry)
		if !s.expired(e, now) {
			break
		}
		if counts[e.binding.AuthID]--; counts[e.binding.AuthID] <= 0 {
			delete(counts, e.binding.AuthID)
		}
	}
	return counts
}

// Len reports how many bindings are held, including any that have expired but
// not yet been swept.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index)
}

// Sweep removes bindings idle past the TTL and returns how many it removed.
func (s *Store) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ttl <= 0 {
		return 0
	}
	n := 0
	for el := s.order.Front(); el != nil; {
		next := el.Next()
		if s.expired(el.Value.(*entry), now) {
			s.remove(el)
			n++
		}
		el = next
	}
	return n
}

// expired reports whether a binding has been idle longer than the TTL.
func (s *Store) expired(e *entry, now time.Time) bool {
	return s.ttl > 0 && now.Sub(e.binding.LastSeen) > s.ttl
}

// remove unlinks an element from the order, the index and the per-credential
// tally. It is the only way an entry leaves the table, so the tally cannot
// drift. Callers hold the lock.
func (s *Store) remove(el *list.Element) {
	e := el.Value.(*entry)
	delete(s.index, e.index)
	s.order.Remove(el)
	if s.counts[e.binding.AuthID]--; s.counts[e.binding.AuthID] <= 0 {
		delete(s.counts, e.binding.AuthID)
	}
}

// evict drops least-recently-seen bindings until the table is within cap.
// Callers hold the lock.
func (s *Store) evict() {
	if s.max <= 0 {
		return
	}
	for len(s.index) > s.max {
		el := s.order.Back()
		if el == nil {
			return
		}
		s.remove(el)
	}
}
