package session

import (
	"container/list"
	"sort"
	"sync"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// Store holds the conversation-to-credential bindings.
//
// Every method is safe for concurrent use, because the pick path runs with no
// serialization from the host. There is no background goroutine: expiry is
// driven by the caller through Sweep, and by Lookup dropping the entry it
// happens to touch. A goroutine here would outlive nothing the host can
// cancel and would block dlclose.
//
// Keys are opaque to the Store. Callers pass whatever BindingKey composes.
type Store struct {
	mu  sync.Mutex
	ttl time.Duration
	max int
	// order is the access order, most recently seen at the front, which is
	// what eviction and lookup need in O(1).
	order *list.List
	index map[string]*list.Element
}

// NewStore returns an empty store. ttl bounds idle time rather than total
// session length, so reuse refreshes it; maxSessions caps the table, evicting
// the least recently seen. A non-positive ttl disables expiry and a
// non-positive maxSessions disables the cap, neither of which
// model.Config.Normalize permits.
func NewStore(ttl time.Duration, maxSessions int) *Store {
	return &Store{
		ttl:   ttl,
		max:   maxSessions,
		order: list.New(),
		index: make(map[string]*list.Element),
	}
}

// Lookup returns the binding for a key. A hit refreshes LastSeen and counts a
// hit; a binding idle past the TTL is a miss and is dropped.
func (s *Store) Lookup(key string, now time.Time) (model.Binding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.index[key]
	if !ok {
		return model.Binding{}, false
	}
	b := el.Value.(*model.Binding)
	if s.expired(b, now) {
		s.remove(el)
		return model.Binding{}, false
	}
	b.LastSeen = now
	b.Hits++
	s.order.MoveToFront(el)
	return *b, true
}

// Bind pins a key to a credential and returns the resulting binding. Rebinding
// to the same credential and model refreshes the existing binding; anything
// else replaces it, because a new credential means a new cached prefix.
func (s *Store) Bind(key, authID, modelID string, now time.Time) model.Binding {
	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.index[key]; ok {
		b := el.Value.(*model.Binding)
		if b.AuthID == authID && b.Model == modelID {
			b.LastSeen = now
			s.order.MoveToFront(el)
			return *b
		}
		s.remove(el)
	}

	b := &model.Binding{
		SessionKey: key,
		AuthID:     authID,
		Model:      modelID,
		BoundAt:    now,
		LastSeen:   now,
	}
	s.index[key] = s.order.PushFront(b)
	s.evict()
	return *b
}

// Drop removes one binding.
func (s *Store) Drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.index[key]; ok {
		s.remove(el)
	}
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
		if el.Value.(*model.Binding).AuthID == authID {
			s.remove(el)
			n++
		}
		el = next
	}
	return n
}

// All returns every binding, newest LastSeen first and ties broken by key, so
// the status UI renders the same table twice for the same state.
func (s *Store) All() []model.Binding {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]model.Binding, 0, len(s.index))
	for el := s.order.Front(); el != nil; el = el.Next() {
		out = append(out, *el.Value.(*model.Binding))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].SessionKey < out[j].SessionKey
	})
	return out
}

// Len reports how many bindings are held, including any that have expired but
// not yet been swept.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if s.expired(el.Value.(*model.Binding), now) {
			s.remove(el)
			n++
		}
		el = next
	}
	return n
}

// expired reports whether a binding has been idle longer than the TTL.
func (s *Store) expired(b *model.Binding, now time.Time) bool {
	return s.ttl > 0 && now.Sub(b.LastSeen) > s.ttl
}

// remove unlinks an element from both the order and the index. Callers hold
// the lock.
func (s *Store) remove(el *list.Element) {
	delete(s.index, el.Value.(*model.Binding).SessionKey)
	s.order.Remove(el)
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
