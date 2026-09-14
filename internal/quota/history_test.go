package quota

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func sessionAt(util float64, resets time.Time) model.Window {
	return model.Window{Kind: model.WindowSession, Utilization: util, ResetsAt: resets, Duration: model.SessionDuration}
}

func historyOf(t *testing.T, s *Store, id string, kind model.WindowKind) model.WindowHistory {
	t.Helper()
	for _, h := range s.History(id, 0) {
		if h.Kind == kind && h.Scope == "" {
			return h
		}
	}
	t.Fatalf("no %s history for %s", kind, id)
	return model.WindowHistory{}
}

func TestHistoryRecordsEveryMergedReading(t *testing.T) {
	s := NewStore()
	resets := testNow.Add(3 * time.Hour)
	s.Put(endpointSnapshot("auth-1", testNow, sessionAt(0.10, resets), weeklyWindow(0.2)))
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.12, resets)}, testNow.Add(2*time.Minute))
	s.Put(endpointSnapshot("auth-1", testNow.Add(4*time.Minute), sessionAt(0.15, resets), weeklyWindow(0.2)))

	h := historyOf(t, s, "auth-1", model.WindowSession)
	if len(h.Cycles) != 1 || len(h.Cycles[0].Samples) != 3 {
		t.Fatalf("history = %+v, want one cycle of three samples", h)
	}
	if got := h.Cycles[0].Samples[1].Utilization; got != 0.12 {
		t.Errorf("header sample = %v, want 0.12", got)
	}
	if !h.Cycles[0].ResetsAt.Equal(resets) {
		t.Errorf("cycle resets_at = %v, want %v", h.Cycles[0].ResetsAt, resets)
	}
	// The weekly window was read twice at one value: two samples bound the
	// flat run, no more.
	if w := historyOf(t, s, "auth-1", model.WindowWeekly); w.Samples() != 2 {
		t.Errorf("weekly samples = %d, want the two ends of a flat run", w.Samples())
	}
}

func TestHistoryFoldsAnIdleRunIntoItsEnds(t *testing.T) {
	s := NewStore()
	resets := testNow.Add(3 * time.Hour)
	for i := 0; i < 30; i++ {
		s.Put(endpointSnapshot("auth-1", testNow.Add(time.Duration(i)*2*time.Minute), sessionAt(0.40, resets)))
	}
	h := historyOf(t, s, "auth-1", model.WindowSession)
	if h.Samples() != 2 {
		t.Fatalf("samples = %d, want 2 for a flat run", h.Samples())
	}
	last := h.Cycles[0].Samples[1]
	if !last.At.Equal(testNow.Add(58 * time.Minute)) {
		t.Errorf("run end = %v, want the newest reading", last.At)
	}
	s.Put(endpointSnapshot("auth-1", testNow.Add(time.Hour), sessionAt(0.41, resets)))
	if h := historyOf(t, s, "auth-1", model.WindowSession); h.Samples() != 3 {
		t.Errorf("samples = %d, want the run plus a changed reading", h.Samples())
	}
}

func TestHistoryReplacesAReadingWithinAMinute(t *testing.T) {
	s := NewStore()
	resets := testNow.Add(3 * time.Hour)
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.10, resets)}, testNow)
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.11, resets)}, testNow.Add(20*time.Second))
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.13, resets)}, testNow.Add(40*time.Second))
	h := historyOf(t, s, "auth-1", model.WindowSession)
	if h.Samples() != 1 || h.Cycles[0].Samples[0].Utilization != 0.13 {
		t.Errorf("history = %+v, want the newest reading of the minute alone", h)
	}
	// Out of order readings never walk the ring backwards.
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.05, resets)}, testNow.Add(-time.Minute))
	if h := historyOf(t, s, "auth-1", model.WindowSession); h.Samples() != 1 {
		t.Errorf("samples = %d after a stale reading, want 1", h.Samples())
	}
}

func TestHistoryOpensACycleAtAReset(t *testing.T) {
	s := NewStore()
	first := testNow.Add(time.Hour)
	s.Put(endpointSnapshot("auth-1", testNow, sessionAt(0.90, first)))
	s.Put(endpointSnapshot("auth-1", testNow.Add(30*time.Minute), sessionAt(0.98, first)))
	// Clock skew inside the tolerance is the same window.
	s.Put(endpointSnapshot("auth-1", testNow.Add(50*time.Minute), sessionAt(0.99, first.Add(time.Minute))))
	second := first.Add(model.SessionDuration)
	s.Put(endpointSnapshot("auth-1", testNow.Add(62*time.Minute), sessionAt(0.02, second)))
	s.Put(endpointSnapshot("auth-1", testNow.Add(64*time.Minute), sessionAt(0.05, second)))

	h := historyOf(t, s, "auth-1", model.WindowSession)
	if len(h.Cycles) != 2 {
		t.Fatalf("cycles = %d, want the boundary at the reset", len(h.Cycles))
	}
	if len(h.Cycles[0].Samples) != 3 || len(h.Cycles[1].Samples) != 2 {
		t.Errorf("cycle sizes = %d, %d; want 3, 2", len(h.Cycles[0].Samples), len(h.Cycles[1].Samples))
	}
	if !h.Cycles[0].ResetsAt.Equal(first) || !h.Cycles[1].ResetsAt.Equal(second) {
		t.Errorf("cycle resets = %v, %v", h.Cycles[0].ResetsAt, h.Cycles[1].ResetsAt)
	}
}

func TestHistoryCoarsensAndCaps(t *testing.T) {
	r := &ring{}
	resets := testNow.Add(10 * 24 * time.Hour)
	// A reading every minute for nine days, every one different.
	n := 9 * 24 * 60
	for i := 0; i < n; i++ {
		r.record(testNow.Add(time.Duration(i)*time.Minute), model.Window{Kind: model.WindowWeekly, Utilization: float64(i) / float64(n), ResetsAt: resets})
	}
	h := r.export(model.WindowWeekly, "", 0)
	if h.Samples() > historyMaxSamples {
		t.Fatalf("samples = %d, over the cap %d", h.Samples(), historyMaxSamples)
	}
	if h.Samples() < historyMaxSamples-20 {
		t.Errorf("samples = %d, want the ring near full", h.Samples())
	}
	samples := h.Cycles[0].Samples
	newest := samples[len(samples)-1].At
	fine, coarse := 0, 0
	for i := 1; i < len(samples); i++ {
		gap := samples[i].At.Sub(samples[i-1].At)
		if samples[i].At.After(newest.Add(-historyFineSpan)) {
			fine++
			if gap != historyFineStep {
				t.Fatalf("fine-tier gap = %v at %d", gap, i)
			}
		} else {
			coarse++
			// The bucket the fine edge is passing through is partial, so a
			// coarse gap is at most one bucket and at least one step.
			if gap < historyFineStep || gap > historyCoarseStep {
				t.Fatalf("coarse-tier gap = %v at %d", gap, i)
			}
		}
	}
	if avg := newest.Add(-historyFineSpan).Sub(samples[0].At) / time.Duration(coarse); avg < historyCoarseStep-time.Minute {
		t.Errorf("average coarse spacing = %v, want about %v", avg, historyCoarseStep)
	}
	if fine != 24*60 {
		t.Errorf("fine samples = %d, want a day of minutes", fine)
	}
	// 2600 - 1441 fine = 1159 coarse samples at 10 minutes ≈ 8 days: more than
	// the week the window spans.
	if span := newest.Sub(samples[0].At); span < 7*24*time.Hour {
		t.Errorf("history spans %v, want at least the full window", span)
	}
	if coarse == 0 {
		t.Error("no coarse samples")
	}
	if got := r.export(model.WindowWeekly, "", HistoryPublishMax).Samples(); got > HistoryPublishMax+1 {
		t.Errorf("published samples = %d, want at most %d plus the cycle's last", got, HistoryPublishMax)
	}
}

func TestHistoryCapEvictsWholeOldCycles(t *testing.T) {
	r := &ring{}
	for c := 0; c < 3; c++ {
		resets := testNow.Add(time.Duration(c+1) * model.SessionDuration)
		for i := 0; i < historyMaxSamples; i++ {
			r.record(testNow.Add(time.Duration(c*historyMaxSamples+i)*time.Minute), model.Window{Kind: model.WindowSession, Utilization: float64(i%7) / 10, ResetsAt: resets})
		}
	}
	h := r.export(model.WindowSession, "", 0)
	if h.Samples() > historyMaxSamples {
		t.Errorf("samples = %d, over the cap", h.Samples())
	}
	for _, c := range h.Cycles {
		if len(c.Samples) == 0 {
			t.Error("an emptied cycle survived eviction")
		}
	}
}

func TestHistoryIsPrunedWithTheSnapshot(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.1)))
	s.Put(endpointSnapshot("auth-2", testNow, sessionWindow(0.2)))
	s.Prune(map[string]struct{}{"auth-2": {}})
	if s.History("auth-1", 0) != nil {
		t.Error("pruned credential still has history")
	}
	if len(s.History("auth-2", 0)) != 1 {
		t.Error("kept credential lost its history")
	}
	if s.History("auth-9", 0) != nil {
		t.Error("unknown credential reports history")
	}
}

func TestHistoryPublishedRowsCarryNoIdentity(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.1)))
	b, err := json.Marshal(s.History("auth-1", 0))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"kind":"five_hour","cycles":[{"resets_at":"2026-09-04T21:00:00Z","samples":[[` + strconv.FormatInt(testNow.Unix(), 10) + `,0.1]]}]}]`
	if string(b) != want {
		t.Errorf("history JSON = %s\nwant %s", b, want)
	}
}

func TestHistorySurvivesSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "history.json")
	s := NewStore()
	first := testNow.Add(time.Hour)
	s.Put(endpointSnapshot("auth-1", testNow, sessionAt(0.90, first), weeklyWindow(0.3)))
	s.Put(endpointSnapshot("auth-1", testNow.Add(2*time.Minute), sessionAt(0.95, first), weeklyWindow(0.31)))
	s.Put(endpointSnapshot("auth-1", testNow.Add(62*time.Minute), sessionAt(0.02, first.Add(model.SessionDuration)), weeklyWindow(0.32)))
	s.Put(endpointSnapshot("auth-2", testNow, sessionWindow(0.5)))
	if err := s.SaveHistory(path); err != nil {
		t.Fatal(err)
	}

	// A fresh process: the file loads before any reading, and the next poll's
	// reading extends what was loaded rather than starting over.
	fresh := NewStore()
	if err := fresh.LoadHistory(path); err != nil {
		t.Fatal(err)
	}
	if len(fresh.All()) != 0 {
		t.Fatal("an import opened a snapshot row on its own")
	}
	fresh.Put(endpointSnapshot("auth-1", testNow.Add(64*time.Minute), sessionAt(0.04, first.Add(model.SessionDuration)), weeklyWindow(0.33)))
	h := historyOf(t, fresh, "auth-1", model.WindowSession)
	if len(h.Cycles) != 2 || len(h.Cycles[0].Samples) != 2 || len(h.Cycles[1].Samples) != 2 {
		t.Errorf("session history after reload = %+v", h)
	}
	if w := historyOf(t, fresh, "auth-1", model.WindowWeekly); w.Samples() != 4 {
		t.Errorf("weekly samples = %d, want the three saved plus one live", w.Samples())
	}
	// A credential the host no longer lists is pruned with its pending import.
	fresh.Prune(map[string]struct{}{"auth-1": {}})
	fresh.Put(endpointSnapshot("auth-2", testNow.Add(65*time.Minute), sessionWindow(0.6)))
	if h := historyOf(t, fresh, "auth-2", model.WindowSession); h.Samples() != 1 {
		t.Errorf("pruned pending history was adopted: %+v", h)
	}

	// A missing file is silence; a bad one is an error and leaves the store
	// alone.
	if err := NewStore().LoadHistory(filepath.Join(t.TempDir(), "none.json")); err != nil {
		t.Errorf("missing file: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := writeFile(bad, `{"version":99,"seats":{}}`); err != nil {
		t.Fatal(err)
	}
	if err := NewStore().LoadHistory(bad); err == nil {
		t.Error("unknown version loaded silently")
	}
}

// TestPrunedHistorySurvivesASave covers the recovery path Prune opens: a seat
// the listing drops keeps its history in pending, and a save taken before the
// seat returns carries that history to disk, so a restart in between does not
// lose it.
func TestPrunedHistorySurvivesASave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.3)))
	s.Put(endpointSnapshot("auth-1", testNow.Add(2*time.Minute), sessionWindow(0.4)))
	s.Put(endpointSnapshot("auth-2", testNow, sessionWindow(0.5)))

	// auth-1 leaves the listing; its history moves to pending.
	s.Prune(map[string]struct{}{"auth-2": {}})
	if err := s.SaveHistory(path); err != nil {
		t.Fatal(err)
	}

	fresh := NewStore()
	if err := fresh.LoadHistory(path); err != nil {
		t.Fatal(err)
	}
	// auth-1 is listed again and reads once; it adopts the two saved samples.
	fresh.Put(endpointSnapshot("auth-1", testNow.Add(4*time.Minute), sessionWindow(0.45)))
	if h := historyOf(t, fresh, "auth-1", model.WindowSession); h.Samples() != 3 {
		t.Errorf("session samples after prune, save, load, return = %d, want 3", h.Samples())
	}
}

func TestImportBehindLiveReadings(t *testing.T) {
	resets := testNow.Add(3 * time.Hour)
	live := NewStore()
	live.Put(endpointSnapshot("auth-1", testNow.Add(10*time.Minute), sessionAt(0.5, resets)))
	live.ImportHistory(map[string][]model.WindowHistory{"auth-1": {{
		Kind: model.WindowSession,
		Cycles: []model.Cycle{{ResetsAt: resets, Samples: []model.Sample{
			{At: testNow, Utilization: 0.3}, {At: testNow.Add(5 * time.Minute), Utilization: 0.4},
		}}},
	}}})
	h := historyOf(t, live, "auth-1", model.WindowSession)
	if h.Samples() != 3 || h.Cycles[0].Samples[2].Utilization != 0.5 {
		t.Errorf("history = %+v, want the saved samples ahead of the live one", h)
	}
	if v := live.HistoryVersion(); v == 0 {
		t.Error("version did not advance")
	}
}

// An estimated cycle is one reconstructed from a token log. The first observed
// reading into it clears the flag and records where the estimate ends; both
// forms survive a save and a load, and a file written without the fields loads
// as observed throughout.
func TestHistoryEstimatedCycleTurnsObserved(t *testing.T) {
	resets := testNow.Add(3 * time.Hour)
	s := NewStore()
	s.ImportHistory(map[string][]model.WindowHistory{"auth-1": {{
		Kind: model.WindowSession,
		Cycles: []model.Cycle{{ResetsAt: resets, Estimated: true, Samples: []model.Sample{
			{At: testNow, Utilization: 0.3}, {At: testNow.Add(5 * time.Minute), Utilization: 0.4},
		}}},
	}}})
	// The estimate is adopted as one, ahead of any observation.
	s.Put(endpointSnapshot("auth-1", testNow.Add(5*time.Minute+20*time.Second), sessionAt(0.41, resets)))
	h := historyOf(t, s, "auth-1", model.WindowSession)
	c := h.Cycles[0]
	if c.Estimated || c.EstimatedUntil == nil || !c.EstimatedUntil.Equal(testNow.Add(5*time.Minute)) {
		t.Fatalf("cycle after the first observation = %+v, want the estimate ending at its last sample", c)
	}
	// The observation stands on its own, never folded into the estimate's
	// last sample even inside the fine step.
	if len(c.Samples) != 3 || c.SampleEstimated(c.Samples[2]) || !c.SampleEstimated(c.Samples[1]) {
		t.Errorf("samples = %+v, want two estimated then one observed", c.Samples)
	}

	path := filepath.Join(t.TempDir(), "history.json")
	if err := s.SaveHistory(path); err != nil {
		t.Fatal(err)
	}
	fresh := NewStore()
	if err := fresh.LoadHistory(path); err != nil {
		t.Fatal(err)
	}
	fresh.Put(endpointSnapshot("auth-1", testNow.Add(8*time.Minute), sessionAt(0.45, resets)))
	got := historyOf(t, fresh, "auth-1", model.WindowSession).Cycles[0]
	if got.Estimated || got.EstimatedUntil == nil || !got.EstimatedUntil.Equal(*c.EstimatedUntil) || len(got.Samples) != 4 {
		t.Errorf("reloaded cycle = %+v, want the boundary kept and the new reading appended", got)
	}

	// A wholly estimated cycle round-trips with its flag; a file with neither
	// field is observed throughout.
	est := NewStore()
	est.ImportHistory(map[string][]model.WindowHistory{"auth-1": {{
		Kind:   model.WindowSession,
		Cycles: []model.Cycle{{ResetsAt: resets, Estimated: true, Samples: []model.Sample{{At: testNow, Utilization: 0.3}}}},
	}}})
	// The seat's reading opens a new cycle, so the estimated one stays whole.
	est.Put(endpointSnapshot("auth-1", testNow.Add(4*time.Hour), sessionAt(0.02, resets.Add(model.SessionDuration))))
	if err := est.SaveHistory(path); err != nil {
		t.Fatal(err)
	}
	body, _ := readFile(path)
	if !strings.Contains(body, `"estimated":true`) || strings.Contains(body, "estimated_until") {
		t.Errorf("saved file = %s, want the flag and no boundary", body)
	}
	plain := NewStore()
	if err := writeFile(path, `{"version":1,"seats":{"auth-1":[{"kind":"five_hour","cycles":[{"resets_at":"`+resets.Format(time.RFC3339)+`","samples":[[`+epoch(17, 30)+`,0.3]]}]}]}}`); err != nil {
		t.Fatal(err)
	}
	if err := plain.LoadHistory(path); err != nil {
		t.Fatal(err)
	}
	plain.Put(endpointSnapshot("auth-1", testNow.Add(2*time.Minute), sessionAt(0.31, resets)))
	if pc := historyOf(t, plain, "auth-1", model.WindowSession).Cycles[0]; pc.Estimated || pc.EstimatedUntil != nil {
		t.Errorf("a file without the fields loaded as estimated: %+v", pc)
	}
}

// TestHistoryBreaksTheCycleAfterItsResetPasses covers a provider still naming
// the expired reset once the window has rolled: windowReset drops an instant
// that far in the past, leaving the fresh window's near-zero reading nothing
// to roll on, and it would otherwise land in the cycle that just ended and
// draw a plunge to zero instead of a break in the line.
func TestHistoryBreaksTheCycleAfterItsResetPasses(t *testing.T) {
	s := NewStore()
	resets := testNow.Add(5 * time.Minute)
	s.Put(endpointSnapshot("auth-1", testNow, sessionAt(0.90, resets)))

	// Three minutes past the reset, and the provider's stale instant is gone.
	after := resets.Add(3 * time.Minute)
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.001, time.Time{})}, after)

	h := historyOf(t, s, "auth-1", model.WindowSession)
	if len(h.Cycles) != 2 {
		t.Fatalf("cycles = %+v, want the post-reset reading in a cycle of its own", h.Cycles)
	}
	if n := len(h.Cycles[0].Samples); n != 1 || h.Cycles[0].Samples[0].Utilization != 0.90 {
		t.Errorf("ended cycle = %+v, want only the pre-reset reading", h.Cycles[0].Samples)
	}

	// The reading that finally carries the real reset rolls the fresh cycle
	// onto it rather than opening one per reading.
	next := resets.Add(model.SessionDuration)
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.004, time.Time{})}, after.Add(2*time.Minute))
	s.MergeHeaders("auth-1", []model.Window{sessionAt(0.02, next)}, after.Add(4*time.Minute))
	h = historyOf(t, s, "auth-1", model.WindowSession)
	if len(h.Cycles) != 2 {
		t.Fatalf("cycles = %d, want the fresh cycle to absorb its own readings", len(h.Cycles))
	}
	if !h.Cycles[1].ResetsAt.Equal(next) {
		t.Errorf("fresh cycle resets_at = %v, want %v", h.Cycles[1].ResetsAt, next)
	}

	// A reading inside the tolerance still belongs to the cycle it names, so
	// clock skew alone never splits one.
	skew := NewStore()
	skew.Put(endpointSnapshot("auth-2", testNow, sessionAt(0.90, resets)))
	skew.MergeHeaders("auth-2", []model.Window{sessionAt(0.91, resets)}, resets.Add(time.Minute))
	if h := historyOf(t, skew, "auth-2", model.WindowSession); len(h.Cycles) != 1 {
		t.Errorf("cycles = %+v, want one; a reading within the tolerance split the cycle", h.Cycles)
	}
}
