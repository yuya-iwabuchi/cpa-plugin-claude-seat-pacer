package quota

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// Store holds the newest snapshot per credential and is safe for concurrent
// use. Every read hands back a copy, so a caller on the pick path cannot
// mutate stored state and needs no lock of its own.
type Store struct {
	mu      sync.RWMutex
	entries map[string]*entry
	// pending holds the recorded histories of credentials without an entry:
	// ones imported before the credential's first reading and ones a prune
	// dropped. Only their history and order are read. The entry adopts them
	// when its first reading arrives, so an import never opens a row the host
	// has not listed.
	pending map[string]*entry
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
	// history holds every window's recorded utilization and refusal spans, in
	// the order the windows were first seen so exports are stable.
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

// exportAll copies every window's whole recorded history as of now, in the
// order the windows were first seen.
func (e *entry) exportAll(now time.Time) []model.WindowHistory {
	out := make([]model.WindowHistory, 0, len(e.order))
	for _, key := range e.order {
		out = append(out, e.history[key].export(key.kind, key.scope, 0, now))
	}
	return out
}

func (e *entry) copy() model.AuthSnapshot {
	out := e.snap
	out.Windows = slices.Clone(e.snap.Windows)
	return out
}

// mergeHeaderReading folds a header window into a stored one. The headers
// report no severity and may omit status, so those two fields keep whatever a
// usage-endpoint read established and a merge cannot blank a window the
// endpoint called critical or rejected. A reading with no reset instant, one
// the header omitted or carried out of range, keeps the stored instant, which
// is what places the window on the pace curve.
func mergeHeaderReading(dst *model.Window, src model.Window) {
	dst.Utilization = src.Utilization
	dst.Active = src.Active
	if !src.ResetsAt.IsZero() {
		dst.ResetsAt = src.ResetsAt
	}
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
	return &Store{entries: make(map[string]*entry), pending: make(map[string]*entry)}
}

// adopt seeds a new entry's history from a pending one.
func (s *Store) adopt(id string, e *entry) {
	p, ok := s.pending[id]
	if !ok {
		return
	}
	delete(s.pending, id)
	e.history, e.order = p.history, p.order
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
// the header's utilization, Active flag and any reset it carries, and keeps the
// severity and the status the headers do not report, so an endpoint verdict of
// critical or rejected still reaches Window.Blocking after a merge.
//
// At most one window is Active. A merged window that claims the flag clears it
// on every other window in the snapshot, so the binding window the provider
// names stays unambiguous.
//
// A reading older than what the store already holds for a window is dropped,
// so responses that land out of order cannot walk a window backwards. A
// reading that rolls or clears a window's history ends its ongoing refusal
// span, as reset or as cleared; it opens none, which is RecordRefusals' part.
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
		if !w.Derived {
			e.observe(observedAt, w)
		}
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

// RecordRefusals records the provider refusing a credential's windows: every
// window in windows that its reading reports as rejected, derived family caps
// included, gains a refusal at `at` of the request admitted at `admitted`,
// which is `at` when zero. A refusal is expected to last until the reset its
// reading names, else the stored window's reset, else `at` plus the window's
// length. A status kept from an endpoint read records none, since only the
// readings given count. Refusals are recorded whatever MergeHeaders made of
// the same readings, so a refusal that lands behind a fresher reading still
// counts, while one admitted at or before a served request the window caps
// changes nothing. The history version advances only when a span changed.
func (s *Store) RecordRefusals(authID string, windows []model.Window, admitted, at time.Time) {
	if authID == "" || at.IsZero() {
		return
	}
	if admitted.IsZero() {
		admitted = at
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[authID]
	if !ok {
		return
	}
	for _, w := range windows {
		if !w.Blocking() {
			continue
		}
		key := keyOf(w)
		r, has := e.history[key]
		if !has {
			r = &ring{}
		}
		if !r.refuse(admitted, at, refusedUntil(w, e.snap.Windows, at)) {
			continue
		}
		if !has {
			e.history[key] = r
			e.order = append(e.order, key)
		}
		s.history++
	}
}

// refusedUntil reports when a refusal of w observed at `at` is expected to
// end: the reset w names, else the reset of the stored window with w's
// identity, else `at` plus the window's length. It is zero when none is known.
func refusedUntil(w model.Window, stored []model.Window, at time.Time) time.Time {
	reset, length := w.ResetsAt, w.Duration
	if i := slices.IndexFunc(stored, hasKey(keyOf(w))); i >= 0 {
		if reset.IsZero() {
			reset = stored[i].ResetsAt
		}
		if length <= 0 {
			length = stored[i].Duration
		}
	}
	if reset.IsZero() && length > 0 {
		return at.Add(length)
	}
	return reset
}

// MarkServed records a request for a model of family that a credential's
// windows admitted at `admitted` and began answering at `at`. Every window
// bearing on that family keeps the admission, so a refusal admitted at or
// before it and delivered later changes nothing, and ends its ongoing
// refusal span at `at` as served when the request was admitted after the
// span's latest refusal; a request admitted at or before it was in flight
// while the window refused and ends nothing. A span on any other family's
// cap keeps running. The history version advances only when a span changed.
func (s *Store) MarkServed(authID, family string, admitted, at time.Time) {
	if authID == "" || at.IsZero() {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.entries[authID]
	if !ok {
		return
	}
	changed := false
	for _, key := range e.order {
		if (model.Window{Kind: key.kind, Scope: key.scope}).BearsOn(family) && e.history[key].serve(admitted, at) {
			changed = true
		}
	}
	if changed {
		s.history++
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
//
// A dropped credential's recorded history moves to pending rather than going
// with its entry, so a credential absent from one listing and back in the next
// adopts the history it already had, refusal and served admissions included,
// instead of restarting from empty. A pending history whose credential the
// following prune does not keep is forgotten, which bounds what a departed
// credential holds.
func (s *Store) Prune(keep map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id := range s.pending {
		if _, ok := keep[id]; !ok {
			delete(s.pending, id)
		}
	}
	for id, e := range s.entries {
		if _, ok := keep[id]; ok {
			continue
		}
		if len(e.order) > 0 {
			s.pending[id] = e
		}
		delete(s.entries, id)
		s.history++
	}
}

// History reports a credential's recorded utilization as of now, one entry
// per window in the order the windows were first seen, thinned to at most max
// samples per window. Every refusal span is kept whatever max is, and an
// ongoing one whose expected reset is not after now reads as ended by it.
// Nil for an unknown credential.
func (s *Store) History(authID string, max int, now time.Time) []model.WindowHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[authID]
	if !ok {
		return nil
	}
	out := make([]model.WindowHistory, 0, len(e.order))
	for _, key := range e.order {
		out = append(out, e.history[key].export(key.kind, key.scope, max, now))
	}
	return out
}

// HistoryVersion counts the changes made to the recorded histories so far.
func (s *Store) HistoryVersion() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.history
}

// ExportHistory copies every credential's whole recorded history as of now,
// keyed by credential id, which is the form the history file holds. An
// ongoing refusal span whose expected reset is not after now reads as ended
// by it, as History reads it. A pending history, imported ahead of the
// credential's first reading or held since a prune dropped it, is a
// credential's too: it is what the credential adopts on its return, and a
// save before the return must not lose it.
func (s *Store) ExportHistory(now time.Time) map[string][]model.WindowHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string][]model.WindowHistory, len(s.entries)+len(s.pending))
	for id, p := range s.pending {
		out[id] = p.exportAll(now)
	}
	for id, e := range s.entries {
		out[id] = e.exportAll(now)
	}
	return out
}

// ImportHistory holds stored histories, loaded at `loaded`, for the readings
// still to come. A credential whose history the store already holds, listed
// or pending, keeps its samples and gains the stored ones behind them; one
// the store holds nothing for adopts its history when its first reading
// arrives, so an import never opens a row the host has not listed. Every
// replayed sample passes through the same spacing, tiering and cap a live
// reading does, and live refusal spans merge into the stored ones. A request admitted after `loaded` is after every stored
// refusal, so it ends a stored span still ongoing.
func (s *Store) ImportHistory(saved map[string][]model.WindowHistory, loaded time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, hs := range saved {
		if id == "" {
			continue
		}
		e := s.entries[id]
		if e == nil {
			e = s.pending[id]
		}
		if e == nil {
			e = &entry{history: make(map[windowKey]*ring, len(hs))}
			s.pending[id] = e
		}
		for _, h := range hs {
			key := windowKey{kind: h.Kind, scope: h.Scope}
			r := &ring{}
			r.replay(h, loaded)
			if live, has := e.history[key]; has {
				r.addCycles(h.Kind, h.Scope, live.cycles)
				if r.fall == nil {
					r.fall = live.fall.after(r)
				}
				for _, l := range live.locks {
					r.mergeLock(l)
				}
				if len(live.locks) > 0 {
					r.refused = live.refused
				}
				r.served = live.served
				r.compact()
			} else {
				e.order = append(e.order, key)
			}
			e.history[key] = r
		}
	}
	s.history++
}
