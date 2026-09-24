package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// lockSpan is one stretch in which the provider refused a seat's requests on
// one window, as the locks file records it: a JSON object keyed by credential
// id, each holding a list of spans. CutBySuccess marks a span a served
// request ended; any other ran to its reset.
type lockSpan struct {
	Kind         model.WindowKind `json:"kind"`
	Scope        string           `json:"scope"`
	From         int64            `json:"from"`
	To           int64            `json:"to"`
	CutBySuccess bool             `json:"cut_by_success"`
}

// replay replaces the fixture's credentials with the seats of a recorded
// history file as of now: one row per seat, labelled by its file name, its
// history published through a real store, and its present reading taken from
// the newest cycle of each window. Each window keeps the refusal spans the
// file records, plus those locksPath names when it is set.
func (f *fixture) replay(historyPath, locksPath string, now time.Time) error {
	var file struct {
		Seats map[string][]model.WindowHistory `json:"seats"`
	}
	if err := readJSON(historyPath, &file); err != nil {
		return err
	}
	locks := map[string][]lockSpan{}
	if locksPath != "" {
		if err := readJSON(locksPath, &locks); err != nil {
			return err
		}
	}

	f.auths = nil
	f.snapshots = map[string]model.AuthSnapshot{}
	f.snapshotsPast = map[string]model.AuthSnapshot{}
	f.observedAge = map[string]time.Duration{}
	f.bindings = nil
	f.decisions = nil
	f.replayed = map[string][]model.WindowHistory{}

	ids := make([]string, 0, len(file.Seats))
	for id := range file.Seats {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for i, id := range ids {
		label := strings.TrimSuffix(strings.TrimPrefix(id, "claude-"), ".json")
		saved := file.Seats[id]
		for j := range saved {
			saved[j].Locks = mergeLocks(saved[j].Locks, locksOf(locks[id], saved[j].Kind, saved[j].Scope))
		}
		saved = before(saved, now)
		snap := model.AuthSnapshot{
			AuthID: id, AuthIndex: fmt.Sprint(i), Label: label,
			Source: model.SourceUsageEndpoint, ObservedAt: now,
		}
		// The spans go in after the snapshot, so the reading Put records at
		// now, which opens a cycle where the file's newest one has ended,
		// ends none of the spans running at now.
		spans := make([]model.WindowHistory, len(saved))
		for j, h := range saved {
			if w, ok := presentWindow(h, now); ok {
				snap.Windows = append(snap.Windows, w)
			}
			spans[j] = model.WindowHistory{Kind: h.Kind, Scope: h.Scope, Locks: h.Locks}
			saved[j].Locks = nil
		}
		store := quota.NewStore()
		store.ImportHistory(map[string][]model.WindowHistory{id: saved}, now)
		store.Put(snap)
		store.ImportHistory(map[string][]model.WindowHistory{id: spans}, now)
		f.replayed[id] = store.History(id, quota.HistoryPublishMax, now)
		f.snapshots[id] = snap
		f.auths = append(f.auths, model.AuthStatus{
			AuthID: id, Label: label, Name: id,
			Provider: "claude", Priority: 10, HostStatus: "active",
		})
	}
	return nil
}

// presentWindow is a window's reading at now as its newest cycle leaves it: the
// last sample's utilization under the cycle's reset. A reset already passed
// rolls forward by whole windows to an empty one. A refusal span covering now
// marks the window refused.
func presentWindow(h model.WindowHistory, now time.Time) (model.Window, bool) {
	var last *model.Cycle
	for i := len(h.Cycles) - 1; i >= 0; i-- {
		if len(h.Cycles[i].Samples) > 0 && !h.Cycles[i].ResetsAt.IsZero() {
			last = &h.Cycles[i]
			break
		}
	}
	if last == nil {
		return model.Window{}, false
	}
	dur := windowLength(h.Kind)
	w := model.Window{
		Kind: h.Kind, Scope: h.Scope, Duration: dur,
		Utilization: last.Samples[len(last.Samples)-1].Utilization,
		ResetsAt:    last.ResetsAt,
		Status:      model.StatusAllowed, Severity: model.SeverityNormal,
		Active: h.Kind == model.WindowSession,
	}
	for !w.ResetsAt.After(now) {
		w.ResetsAt, w.Utilization = w.ResetsAt.Add(dur), 0
	}
	for _, l := range h.Locks {
		if l.From <= now.Unix() && now.Unix() < l.To {
			w.Status, w.Severity = model.StatusRejected, model.SeverityCritical
		}
	}
	return w, true
}

func windowLength(kind model.WindowKind) time.Duration {
	if kind == model.WindowSession {
		return model.SessionDuration
	}
	return model.WeeklyDuration
}

// mergeLocks is the spans of a and b, oldest first, each stretch once: of two
// spans with the same ends, a's is kept.
func mergeLocks(a, b []model.Lock) []model.Lock {
	out := slices.Concat(a, b)
	slices.SortStableFunc(out, func(x, y model.Lock) int { return cmp.Or(cmp.Compare(x.From, y.From), cmp.Compare(x.To, y.To)) })
	return slices.CompactFunc(out, func(x, y model.Lock) bool { return x.From == y.From && x.To == y.To })
}

// locksOf is the spans on one window, in the locks file's order.
func locksOf(spans []lockSpan, kind model.WindowKind, scope string) []model.Lock {
	var out []model.Lock
	for _, s := range spans {
		if s.Kind == kind && s.Scope == scope && s.To > s.From {
			end := model.LockEndReset
			if s.CutBySuccess {
				end = model.LockEndServed
			}
			out = append(out, model.Lock{From: s.From, To: s.To, End: end})
		}
	}
	return out
}

func readJSON(path string, v any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// before is a history as it stood at now: only the samples taken at or before
// now, and only the refusal spans that had begun by then. A span still
// running at now is ongoing, running to its window's reset at now, as the
// live store publishes it, or for one window's length from its start when
// the history names no reset.
func before(hs []model.WindowHistory, now time.Time) []model.WindowHistory {
	out := make([]model.WindowHistory, 0, len(hs))
	for _, h := range hs {
		h.Cycles = append([]model.Cycle(nil), h.Cycles...)
		kept := h.Cycles[:0]
		for _, c := range h.Cycles {
			var ss []model.Sample
			for _, smp := range c.Samples {
				if !smp.At.After(now) {
					ss = append(ss, smp)
				}
			}
			if len(ss) > 0 {
				c.Samples = ss
				kept = append(kept, c)
			}
		}
		h.Cycles = kept
		h.Locks = slices.DeleteFunc(slices.Clone(h.Locks), func(l model.Lock) bool { return l.From > now.Unix() })
		w, ok := presentWindow(h, now)
		for i, l := range h.Locks {
			if now.Unix() < l.To {
				to := time.Unix(l.From, 0).Add(windowLength(h.Kind))
				if ok {
					to = w.ResetsAt
				}
				h.Locks[i] = model.Lock{From: l.From, To: to.Unix()}
			}
		}
		out = append(out, h)
	}
	return out
}
