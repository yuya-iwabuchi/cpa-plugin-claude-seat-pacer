package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// boostsFileName is the file, beside the history file, that carries the live
// boosts from one run to the next. Its name does not end in .json for the
// reason bindingsFileName's does not.
const boostsFileName = "boosts.state"

// boostsFileVersion is the format of the boosts file; a file of another
// version is ignored rather than misread.
const boostsFileVersion = 1

// boostsFile is the on-disk form of the boost table.
type boostsFile struct {
	Version int           `json:"version"`
	Boosts  []model.Boost `json:"boosts"`
}

// ErrNoReset is the error Boost returns for a BoostReset span on a credential
// whose reading names no reset to last until.
var ErrNoReset = errors.New("the seat's reading names no reset")

// ErrUnknownSpan is the error Boost returns for a span it does not name.
var ErrUnknownSpan = errors.New("unknown boost span")

// boostStore holds the live boosts by credential id. It locks internally, so
// a pick reads it without the plugin's mutex. saveMu is held across each
// change and the file write that follows it, and across a restore, so the
// file and the table never disagree once a change returns unless the write
// failed, which unsaved records.
type boostStore struct {
	mu     sync.Mutex
	boosts map[string]model.Boost
	// missed holds the boosted credentials the latest listing left out.
	missed map[string]struct{}
	saveMu sync.Mutex
	// unsaved marks a table the boosts file does not hold because its last
	// write failed. saveMu guards it.
	unsaved bool
}

func newBoostStore() *boostStore {
	return &boostStore{boosts: make(map[string]model.Boost)}
}

// active returns the boost on authID that holds at now.
func (s *boostStore) active(authID string, now time.Time) (model.Boost, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boosts[authID]
	return b, ok && b.Active(now)
}

// boosted is active as the predicate pace.ApplyBoosts takes.
func (s *boostStore) boosted(now time.Time) func(string) bool {
	return func(id string) bool {
		_, ok := s.active(id, now)
		return ok
	}
}

// set records b, replacing any boost on its credential.
func (s *boostStore) set(b model.Boost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boosts[b.AuthID] = b
}

// clear drops the boost on authID and reports whether there was one.
func (s *boostStore) clear(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.boosts[authID]
	delete(s.boosts, authID)
	return ok
}

// live returns every boost that holds at now, by credential id, and drops the
// ones that ended.
func (s *boostStore) live(now time.Time) []model.Boost {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Boost, 0, len(s.boosts))
	for id, b := range s.boosts {
		if !b.Active(now) {
			delete(s.boosts, id)
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthID < out[j].AuthID })
	return out
}

// prune drops every boost on a credential that neither keep nor the listing
// before it holds, the one listing's grace quota.Store.Prune gives a
// credential's history, and reports whether it dropped any.
func (s *boostStore) prune(keep map[string]struct{}) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped := false
	missed := make(map[string]struct{})
	for id := range s.boosts {
		if _, ok := keep[id]; ok {
			continue
		}
		if _, ok := s.missed[id]; ok {
			delete(s.boosts, id)
			dropped = true
			continue
		}
		missed[id] = struct{}{}
	}
	s.missed = missed
	return dropped
}

// replace swaps the table for boosts that hold at now.
func (s *boostStore) replace(boosts []model.Boost, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boosts = make(map[string]model.Boost, len(boosts))
	for _, b := range boosts {
		if b.AuthID != "" && b.Active(now) {
			s.boosts[b.AuthID] = b
		}
	}
}

// Boost boosts authID for span from now and saves the table. A BoostReset
// span ends at the reset BoostResetAt reads from the credential's latest
// reading.
func (p *Plugin) Boost(authID, span string, now time.Time) (model.Boost, error) {
	b := model.Boost{AuthID: authID, Span: span, SetAt: now}
	switch span {
	case model.BoostHour:
		b.Until = now.Add(time.Hour)
	case model.BoostFourHours:
		b.Until = now.Add(4 * time.Hour)
	case model.BoostUntilCleared:
	case model.BoostReset:
		snap, _ := p.quota.Get(authID)
		b.Until = BoostResetAt(snap, now)
		if b.Until.IsZero() {
			return model.Boost{}, ErrNoReset
		}
	default:
		return model.Boost{}, fmt.Errorf("%w %q", ErrUnknownSpan, span)
	}
	p.boosts.saveMu.Lock()
	defer p.boosts.saveMu.Unlock()
	p.boosts.set(b)
	p.saveBoostsLocked(now)
	return b, nil
}

// Unboost clears the boost on authID, saves the table, and reports whether
// there was one.
func (p *Plugin) Unboost(authID string, now time.Time) bool {
	p.boosts.saveMu.Lock()
	defer p.boosts.saveMu.Unlock()
	ok := p.boosts.clear(authID)
	if ok {
		p.saveBoostsLocked(now)
	}
	return ok
}

// pruneBoosts drops every boost on a credential absent from keep and from
// the listing before it, and saves the table when it dropped one.
func (p *Plugin) pruneBoosts(keep map[string]struct{}, now time.Time) {
	p.boosts.saveMu.Lock()
	defer p.boosts.saveMu.Unlock()
	if p.boosts.prune(keep) {
		p.saveBoostsLocked(now)
	}
}

// BoostResetAt is the reset a BoostReset span lasts until: the all-models
// weekly window's, else the soonest model-family weekly window's, else the
// 5-hour window's when it lies ahead of now, skipping a window that names no
// reset. A weekly reset at or before now has rolled over since the reading,
// so it is carried forward by whole weeks. Zero when the reading names none.
func BoostResetAt(snap model.AuthSnapshot, now time.Time) time.Time {
	var weekly, scoped, session time.Time
	for _, w := range snap.Windows {
		if w.ResetsAt.IsZero() {
			continue
		}
		switch w.Kind {
		case model.WindowWeekly:
			weekly = nextWeeklyReset(w.ResetsAt, now)
		case model.WindowWeeklyScoped:
			if r := nextWeeklyReset(w.ResetsAt, now); scoped.IsZero() || r.Before(scoped) {
				scoped = r
			}
		case model.WindowSession:
			if w.ResetsAt.After(now) {
				session = w.ResetsAt
			}
		}
	}
	switch {
	case !weekly.IsZero():
		return weekly
	case !scoped.IsZero():
		return scoped
	}
	return session
}

// nextWeeklyReset is the first weekly reset after now on the cycle reset
// falls on.
func nextWeeklyReset(reset, now time.Time) time.Time {
	if reset.After(now) {
		return reset
	}
	return reset.Add((now.Sub(reset)/model.WeeklyDuration + 1) * model.WeeklyDuration)
}

// boostsPath is where the boosts file lives: in the history file's directory.
// Empty when no history location is known, which keeps boosts in memory only.
func (p *Plugin) boostsPath() string {
	if p.opts.HistoryFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.opts.HistoryFile), boostsFileName)
}

// saveBoostsLocked writes the live boosts to the boosts file, or removes the
// file when none is live. The caller holds saveMu. A failed write marks the
// table unsaved, and the next change, each poll tick, each configure other
// than a build's register or first configure, plugin.quiesce and a shutdown
// of a build not yet handed over each write an unsaved table again; a
// register or first configure takes the file instead. The failure goes to
// host.log, which a host that has already closed the build's callbacks drops.
func (p *Plugin) saveBoostsLocked(now time.Time) {
	path := p.boostsPath()
	if path == "" {
		return
	}
	live := p.boosts.live(now)
	if len(live) == 0 {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		p.boosts.unsaved = err != nil
		if err != nil {
			p.host.log("warn", "claude-seat-pacer could not remove the boosts file", map[string]any{"error": err.Error()})
		}
		return
	}
	body, err := json.Marshal(boostsFile{Version: boostsFileVersion, Boosts: live})
	if err == nil {
		err = quota.WriteFile(path, body)
	}
	p.boosts.unsaved = err != nil
	if err != nil {
		p.host.log("warn", "claude-seat-pacer could not save the boosts", map[string]any{"error": err.Error()})
	}
}

// retryBoostSave writes a table whose last save failed, and does nothing when
// no save is pending. It runs at each poll tick and each configure that does
// not restore the table, and when the host stops the build, at plugin.quiesce
// and shutdown.
func (p *Plugin) retryBoostSave() {
	p.boosts.saveMu.Lock()
	defer p.boosts.saveMu.Unlock()
	if p.boosts.unsaved {
		p.saveBoostsLocked(p.now())
	}
}

// restoreBoosts replaces the boost table with the boosts file's. It runs when
// a build registers or takes its first configure; from then on the table in
// memory is the build's own, and each change writes it to the file. A host
// that loads the same library again reuses the build's memory and registers
// it again, so the build takes whatever table another build has written
// since. A file that is missing, unreadable, unparseable or of another
// version leaves the table empty.
func (p *Plugin) restoreBoosts(now time.Time) {
	path := p.boostsPath()
	if path == "" {
		return
	}
	p.boosts.saveMu.Lock()
	defer p.boosts.saveMu.Unlock()
	p.boosts.unsaved = false
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		p.boosts.replace(nil, now)
		return
	}
	var f boostsFile
	if err == nil {
		err = json.Unmarshal(body, &f)
	}
	if err == nil && f.Version != boostsFileVersion {
		err = fmt.Errorf("boosts file names version %d", f.Version)
	}
	if err != nil {
		p.host.log("warn", "claude-seat-pacer could not restore the boosts", map[string]any{"error": err.Error()})
		p.boosts.replace(nil, now)
		return
	}
	p.boosts.replace(f.Boosts, now)
}
