package quota

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// Store holds the newest snapshot per credential and is safe for concurrent
// use. Every read hands back a copy, so a caller on the pick path cannot
// mutate stored state and needs no lock of its own.
type Store struct {
	mu      sync.RWMutex
	entries map[string]*entry
	// pending holds imported histories for credentials without an entry yet;
	// the entry adopts them when its first reading arrives, so an import never
	// opens a row the host has not listed.
	pending map[string][]model.WindowHistory
	// history counts every change to the recorded histories, so a writer can
	// tell an unchanged store from one worth flushing.
	history uint64
}

// entry pairs a snapshot with the instant each of its windows was observed.
//
// model.Window carries no timestamp, and a header read covers only the windows
// real traffic touched, so freshness is tracked here instead: a merge advances
// the times of the windows it carries and leaves the rest alone. The
// snapshot's own ObservedAt reports the newest of them, which is what lets a
// merged snapshot read as fresh as its most recent window rather than falling
// back to the age of the last full endpoint read.
type entry struct {
	snap   model.AuthSnapshot
	seenAt map[windowKey]time.Time
	// history holds every window's recorded utilization, in the order the
	// windows were first seen so exports are stable.
	history map[windowKey]*ring
	order   []windowKey
}

// observe records one accepted reading in the window's history.
func (e *entry) observe(at time.Time, w model.Window) {
	key := keyOf(w)
	r, ok := e.history[key]
	if !ok {
		r = &ring{}
		e.history[key] = r
		e.order = append(e.order, key)
	}
	r.record(at, w)
}

func newEntry(snap model.AuthSnapshot, n int) *entry {
	return &entry{snap: snap, seenAt: make(map[windowKey]time.Time, n), history: make(map[windowKey]*ring, n)}
}

func (e *entry) copy() model.AuthSnapshot {
	out := e.snap
	out.Windows = slices.Clone(e.snap.Windows)
	return out
}

// mergeHeaderReading folds a header window into a stored one. The headers
// report no severity and may omit status, so those two fields keep whatever a
// usage-endpoint read established and a merge cannot blank a window the
// endpoint called critical or rejected.
func mergeHeaderReading(dst *model.Window, src model.Window) {
	dst.Utilization = src.Utilization
	dst.ResetsAt = src.ResetsAt
	dst.Active = src.Active
	if src.Duration > 0 {
		dst.Duration = src.Duration
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.Severity != "" {
		dst.Severity = src.Severity
	}
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{entries: make(map[string]*entry), pending: make(map[string][]model.WindowHistory)}
}

// adopt seeds a new entry's history from a pending import.
func (s *Store) adopt(id string, e *entry) {
	hs, ok := s.pending[id]
	if !ok {
		return
	}
	delete(s.pending, id)
	for _, h := range hs {
		key := windowKey{kind: h.Kind, scope: h.Scope}
		r := &ring{}
		r.replay(h)
		e.history[key] = r
		e.order = append(e.order, key)
	}
}

// Put records a usage-endpoint snapshot, replacing the credential's window set
// wholesale: that read covers every window, so a window missing from it is
// gone rather than merely unobserved.
//
// Recency is still per window. A window the store saw more recently than the
// snapshot's ObservedAt keeps its stored value and seen-time, so a header merge
// that landed while the fetch was in flight is not undone by the older reading
// the fetch brings back.
//
// A failed fetch — Err set and no windows — keeps the prior readings and
// updates only Err and ErrCategory, so one endpoint hiccup leaves routing
// sighted. A snapshot with an empty AuthID is dropped, since nothing can
// address it.
func (s *Store) Put(snap model.AuthSnapshot) {
	if snap.AuthID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	prior, exists := s.entries[snap.AuthID]
	if exists && snap.Err != "" && len(snap.Windows) == 0 {
		prior.snap.Err = snap.Err
		prior.snap.ErrCategory = snap.ErrCategory
		if snap.AuthIndex != "" {
			prior.snap.AuthIndex = snap.AuthIndex
		}
		if snap.Label != "" {
			prior.snap.Label = snap.Label
		}
		return
	}

	stored := snap
	stored.Windows = slices.Clone(snap.Windows)
	if exists {
		// Identity comes from config rather than from the endpoint, so a
		// snapshot that omits it inherits what the store already knows.
		if stored.AuthIndex == "" {
			stored.AuthIndex = prior.snap.AuthIndex
		}
		if stored.Label == "" {
			stored.Label = prior.snap.Label
		}
	}

	observedAt := snap.ObservedAt
	newest := observedAt
	e := newEntry(stored, len(stored.Windows))
	if exists {
		e.history, e.order = prior.history, prior.order
	} else {
		s.adopt(snap.AuthID, e)
	}
	for i := range stored.Windows {
		key := keyOf(stored.Windows[i])
		at := observedAt
		kept := false
		if exists {
			if previous, seen := prior.seenAt[key]; seen && previous.After(observedAt) {
				if j := slices.IndexFunc(prior.snap.Windows, hasKey(key)); j >= 0 {
					stored.Windows[i] = prior.snap.Windows[j]
					at = previous
					kept = true
				}
			}
		}
		e.seenAt[key] = at
		if !kept {
			e.observe(at, stored.Windows[i])
		}
		if newest.Before(at) {
			newest = at
		}
	}
	e.snap.ObservedAt = newest
	s.history++
	s.entries[snap.AuthID] = e
}

// MergeHeaders folds a response-header observation into a credential's
// snapshot. Header readings are fresher than an endpoint read but cover only
// the windows traffic touched, so they update the windows they carry and leave
// every other window intact, ObservedAt included.
//
// A window the reading carries is merged field by field, not replaced: it takes
// the header's utilization, reset and Active flag, and keeps the severity and
// the status the headers do not report, so an endpoint verdict of critical or
// rejected still reaches Window.Blocking after a merge.
//
// At most one window is Active. A merged window that claims the flag clears it
// on every other window in the snapshot, so the binding window the provider
// names stays unambiguous.
//
// A reading older than what the store already holds for a window is dropped,
// so responses that land out of order cannot walk a window backwards.
//
// Source keeps naming the endpoint read when one exists, because the snapshot
// still carries endpoint data for windows traffic has not touched. Err is left
// alone too: clearing it here would hide a usage-endpoint failure the poll
// loop is still hitting, and the next successful Put clears it anyway.
func (s *Store) MergeHeaders(authID string, windows []model.Window, observedAt time.Time) {
	if authID == "" || len(windows) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e, exists := s.entries[authID]
	if !exists {
		e = newEntry(model.AuthSnapshot{AuthID: authID, Source: model.SourceResponseHeaders}, len(windows))
		s.adopt(authID, e)
		s.entries[authID] = e
	}
	s.history++

	var activeKey windowKey
	activated := false
	for _, w := range windows {
		key := keyOf(w)
		if previous, seen := e.seenAt[key]; seen && observedAt.Before(previous) {
			continue
		}
		e.seenAt[key] = observedAt
		if i := slices.IndexFunc(e.snap.Windows, hasKey(key)); i >= 0 {
			mergeHeaderReading(&e.snap.Windows[i], w)
		} else {
			e.snap.Windows = append(e.snap.Windows, w)
		}
		e.observe(observedAt, w)
		if w.Active {
			activeKey, activated = key, true
		}
	}
	if activated {
		for i := range e.snap.Windows {
			if keyOf(e.snap.Windows[i]) != activeKey {
				e.snap.Windows[i].Active = false
			}
		}
	}

	if e.snap.ObservedAt.Before(observedAt) {
		e.snap.ObservedAt = observedAt
	}
}

// Get reports a credential's newest snapshot.
func (s *Store) Get(authID string) (model.AuthSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[authID]
	if !ok {
		return model.AuthSnapshot{}, false
	}
	return e.copy(), true
}

// All reports every snapshot ordered by credential id, so the status view and
// the decision log do not reshuffle between reads.
func (s *Store) All() []model.AuthSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]model.AuthSnapshot, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.copy())
	}
	slices.SortFunc(out, func(a, b model.AuthSnapshot) int {
		return strings.Compare(a.AuthID, b.AuthID)
	})
	return out
}

// Prune drops every credential outside keep, which is how a credential removed
// from the host's pool stops appearing in the status view. An empty keep set
// drops all of them.
func (s *Store) Prune(keep map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id := range s.entries {
		if _, ok := keep[id]; !ok {
			delete(s.entries, id)
			s.history++
		}
	}
	for id := range s.pending {
		if _, ok := keep[id]; !ok {
			delete(s.pending, id)
		}
	}
}

// History reports a credential's recorded utilization, one entry per window
// in the order the windows were first seen, thinned to at most max samples
// per window. Nil for an unknown credential.
func (s *Store) History(authID string, max int) []model.WindowHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[authID]
	if !ok {
		return nil
	}
	out := make([]model.WindowHistory, 0, len(e.order))
	for _, key := range e.order {
		out = append(out, e.history[key].export(key.kind, key.scope, max))
	}
	return out
}

// HistoryVersion counts the changes made to the recorded histories so far.
func (s *Store) HistoryVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.history
}

// ExportHistory copies every credential's whole recorded history, keyed by
// credential id, which is the form the history file holds.
func (s *Store) ExportHistory() map[string][]model.WindowHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string][]model.WindowHistory, len(s.entries))
	for id, e := range s.entries {
		hs := make([]model.WindowHistory, 0, len(e.order))
		for _, key := range e.order {
			hs = append(hs, e.history[key].export(key.kind, key.scope, 0))
		}
		out[id] = hs
	}
	return out
}

// ImportHistory holds stored histories for the readings still to come. A
// credential the store already holds keeps its live samples and gains the
// stored ones behind them; one it does not hold yet adopts its history when
// its first reading arrives, so an import never opens a row the host has not
// listed. Every replayed sample passes through the same spacing, tiering and
// cap a live reading does.
func (s *Store) ImportHistory(saved map[string][]model.WindowHistory) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, hs := range saved {
		if id == "" {
			continue
		}
		e, exists := s.entries[id]
		if !exists {
			s.pending[id] = hs
			continue
		}
		for _, h := range hs {
			key := windowKey{kind: h.Kind, scope: h.Scope}
			r := &ring{}
			r.replay(h)
			if live, has := e.history[key]; has {
				for _, c := range live.cycles {
					for _, smp := range c.Samples {
						r.put(smp.At, model.Window{Kind: h.Kind, Scope: h.Scope, Utilization: smp.Utilization, ResetsAt: c.ResetsAt}, c.SampleEstimated(smp))
					}
				}
			} else {
				e.order = append(e.order, key)
			}
			e.history[key] = r
		}
	}
	s.history++
}
