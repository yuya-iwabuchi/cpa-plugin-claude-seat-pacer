package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

var weeklyReset = time.Date(2026, 9, 10, 14, 0, 0, 0, time.UTC)

// heldEntry is a credential the host cools until until.
func heldEntry(until time.Time) HostAuthFileEntry {
	return HostAuthFileEntry{ID: "claude-a.json", AuthIndex: "idx-a", Name: "claude-a.json", Provider: "claude", Unavailable: true, NextRetryAfter: until}
}

// freshSnap is a usage read at testNow with quota left in every window.
func freshSnap(windows ...model.Window) model.AuthSnapshot {
	if len(windows) == 0 {
		windows = []model.Window{
			{Kind: model.WindowSession, Utilization: 0, ResetsAt: testNow.Add(5 * time.Hour)},
			{Kind: model.WindowWeekly, Utilization: 0.02, ResetsAt: weeklyReset},
		}
	}
	return model.AuthSnapshot{AuthID: "claude-a.json", Windows: windows, ObservedAt: testNow, Source: model.SourceUsageEndpoint}
}

// window is one window's history holding the given spans, with the cycle the
// quota store opens where a span ends as cleared or reset.
func window(kind model.WindowKind, scope string, locks ...model.Lock) model.WindowHistory {
	h := model.WindowHistory{Kind: kind, Scope: scope, Locks: locks}
	for _, l := range locks {
		if l.End == model.LockEndCleared || l.End == model.LockEndReset {
			h = reopenAt(h, time.Unix(l.To, 0))
		}
	}
	return h
}

// reopenAt adds a cycle opened at `at` to a window's history.
func reopenAt(h model.WindowHistory, at time.Time) model.WindowHistory {
	h.Cycles = append(h.Cycles, model.Cycle{ResetsAt: weeklyReset, Samples: []model.Sample{{At: at.UTC(), Utilization: 0.02}}})
	return h
}

// ended is a refusal span from `from` that `end` ended at `to`, having
// expected the reset `expected`.
func ended(from, to time.Time, end model.LockEnd, expected time.Time) model.Lock {
	l := model.Lock{From: from.Unix(), To: to.Unix(), End: end}
	if !expected.Equal(to) {
		l.Expected = expected.Unix()
	}
	return l
}

// ongoing is a refusal span from `from` still running to the reset `to`.
func ongoing(from, to time.Time) model.Lock {
	return model.Lock{From: from.Unix(), To: to.Unix()}
}

func TestFindStaleHold(t *testing.T) {
	refused := testNow.Add(-3 * time.Hour)
	cleared := testNow.Add(-time.Hour)
	fableReset := weeklyReset.Add(-20 * time.Hour)
	weekly := func(locks ...model.Lock) []model.WindowHistory {
		return []model.WindowHistory{window(model.WindowWeekly, "", locks...)}
	}
	session := func(locks ...model.Lock) model.WindowHistory { return window(model.WindowSession, "", locks...) }
	fable := func(locks ...model.Lock) model.WindowHistory {
		return window(model.WindowWeeklyScoped, model.FamilyFable, locks...)
	}
	// The host cools until the reset the 429 named, plus up to 30 seconds.
	held := heldEntry(weeklyReset.Add(20 * time.Second))
	clearedSpan := weekly(ended(refused, cleared, model.LockEndCleared, weeklyReset))
	soon := testNow.Add(5 * time.Minute)
	cases := []struct {
		name    string
		entry   HostAuthFileEntry
		snap    model.AuthSnapshot
		history []model.WindowHistory
		want    bool
	}{
		{"a clearing the host still cools past", held, freshSnap(), clearedSpan, true},
		{"a window that rolled early", held, freshSnap(), weekly(ended(refused, cleared, model.LockEndReset, weeklyReset)), true},
		{"a span a served request ended", held, freshSnap(), weekly(ended(refused, cleared, model.LockEndServed, weeklyReset)), false},
		{"a span a served request ended, the window cleared since", held, freshSnap(),
			[]model.WindowHistory{reopenAt(window(model.WindowWeekly, "", ended(refused, cleared, model.LockEndServed, weeklyReset)), cleared.Add(20*time.Minute))}, true},
		{"a span a served request ended, a clearing before it", held, freshSnap(),
			[]model.WindowHistory{reopenAt(window(model.WindowWeekly, "", ended(refused, cleared, model.LockEndServed, weeklyReset)), cleared.Add(-time.Second))}, false},
		{"a 5-hour span", held, freshSnap(), []model.WindowHistory{session(ended(refused, cleared, model.LockEndReset, weeklyReset))}, true},
		{"a 429 on both windows, only the 5-hour one rolled since", held, freshSnap(), []model.WindowHistory{
			reopenAt(session(ended(refused, refused.Add(time.Second), model.LockEndServed, refused.Add(2*time.Hour))), refused.Add(2*time.Hour)),
			window(model.WindowWeekly, "", ended(refused, refused.Add(time.Second), model.LockEndServed, weeklyReset)),
		}, false},
		{"a 429 on both windows, only the 5-hour one rolled since, a full Fable cap", held, freshSnap(), []model.WindowHistory{
			reopenAt(session(ended(refused, refused.Add(time.Second), model.LockEndServed, refused.Add(2*time.Hour))), refused.Add(2*time.Hour)),
			window(model.WindowWeekly, "", ended(refused, refused.Add(time.Second), model.LockEndServed, weeklyReset)),
			fable(ongoing(refused.Add(-time.Hour), weeklyReset)),
		}, false},
		{"a 5-hour refusal with the full Fable cap, after a weekly refusal a served request ended", heldEntry(weeklyReset.Add(20 * time.Second)), freshSnap(), []model.WindowHistory{
			session(ended(refused, cleared, model.LockEndReset, refused.Add(2*time.Hour))),
			window(model.WindowWeekly, "", ended(refused.Add(-2*time.Hour), refused.Add(-2*time.Hour+time.Minute), model.LockEndServed, weeklyReset)),
			fable(ongoing(refused.Add(-3*time.Hour), weeklyReset)),
		}, true},
		{"a 429 on both windows, both reopened since", held, freshSnap(), []model.WindowHistory{
			session(ended(refused, refused.Add(2*time.Hour), model.LockEndReset, refused.Add(2*time.Hour))),
			window(model.WindowWeekly, "", ended(refused, cleared, model.LockEndCleared, weeklyReset)),
		}, true},
		{"a 5-hour refusal that also refused the full Fable cap", heldEntry(fableReset.Add(20 * time.Second)), freshSnap(),
			[]model.WindowHistory{session(ended(refused, cleared, model.LockEndReset, refused.Add(2*time.Hour))), fable(ongoing(refused.Add(-time.Hour), fableReset))}, true},
		{"a Fable refusal begun after the 5-hour one", heldEntry(fableReset.Add(20 * time.Second)), freshSnap(),
			[]model.WindowHistory{session(ended(refused, cleared, model.LockEndReset, refused.Add(2*time.Hour))), fable(ongoing(refused.Add(time.Minute), fableReset))}, false},
		{"a Fable refusal that ended before the 5-hour one began", heldEntry(fableReset.Add(20 * time.Second)), freshSnap(),
			[]model.WindowHistory{session(ended(refused, cleared, model.LockEndReset, refused.Add(2*time.Hour))), fable(ended(refused.Add(-2*time.Hour), refused.Add(-time.Minute), model.LockEndServed, fableReset))}, false},
		{"a reset on schedule", heldEntry(testNow.Add(20 * time.Second)), freshSnap(),
			weekly(ended(refused, testNow.Add(-10*time.Second), model.LockEndReset, testNow.Add(-10*time.Second))), false},
		{"a span that ended just past holdMargin before the cooldown", heldEntry(soon), freshSnap(),
			weekly(ended(refused, soon.Add(-holdMargin-time.Second), model.LockEndCleared, soon)), true},
		{"a span that ended within holdMargin of the cooldown", heldEntry(soon), freshSnap(),
			weekly(ended(refused, soon.Add(-holdMargin+time.Second), model.LockEndCleared, soon)), false},
		{"a cooldown no span expected", heldEntry(testNow.Add(30 * time.Minute)), freshSnap(), clearedSpan, false},
		{"a refusal still ongoing", held, freshSnap(),
			weekly(ended(refused.Add(-time.Hour), refused.Add(-time.Minute), model.LockEndCleared, weeklyReset), ongoing(refused, weeklyReset)), false},
		{"no refusal seen", held, freshSnap(), nil, false},
		{"a later family cap refusal expecting the same reset", held, freshSnap(),
			append(clearedSpan, fable(ongoing(cleared.Add(10*time.Minute), weeklyReset))), false},
		{"an earlier family cap refusal expecting the same reset", held, freshSnap(),
			append(clearedSpan, fable(ended(refused.Add(-time.Hour), refused.Add(-time.Minute), model.LockEndServed, weeklyReset))), true},
		{"a family cap's span alone", held, freshSnap(),
			[]model.WindowHistory{fable(ended(refused, cleared, model.LockEndCleared, weeklyReset))}, false},
		{"a read taken before the span ended", held,
			func() model.AuthSnapshot { s := freshSnap(); s.ObservedAt = cleared.Add(-time.Minute); return s }(),
			clearedSpan, false},
		{"a family cap still full", held,
			freshSnap(model.Window{Kind: model.WindowWeekly, Utilization: 0.02, ResetsAt: weeklyReset},
				model.Window{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 1, ResetsAt: weeklyReset}),
			clearedSpan, true},
		{"a weekly window still full", held,
			freshSnap(model.Window{Kind: model.WindowWeekly, Utilization: 1, ResetsAt: weeklyReset}), clearedSpan, false},
		{"a window the provider still refuses", held,
			freshSnap(model.Window{Kind: model.WindowWeekly, Utilization: 0.02, ResetsAt: weeklyReset, Status: model.StatusRejected}),
			clearedSpan, false},
		{"a read with only a family cap", held,
			freshSnap(model.Window{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0, ResetsAt: weeklyReset}), clearedSpan, false},
		{"a credential the host offers",
			func() HostAuthFileEntry { e := held; e.Unavailable = false; return e }(),
			freshSnap(), clearedSpan, false},
		{"a cooldown already past", heldEntry(testNow.Add(-time.Minute)), freshSnap(), clearedSpan, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hold, got := findStaleHold(tc.entry, tc.snap, tc.history, testNow)
			if got != tc.want {
				t.Fatalf("findStaleHold = %v, want %v", got, tc.want)
			}
			if got && (!hold.until.Equal(tc.entry.NextRetryAfter) || hold.span != refused.Unix()) {
				t.Errorf("hold = %+v", hold)
			}
		})
	}
}

// TestAClearingIsFoundOnItsConfirmingRead drives the quota store through a
// weekly refusal and the two low reads that confirm the provider cleared it:
// the first read alone finds nothing, the second finds the cooldown.
func TestAClearingIsFoundOnItsConfirmingRead(t *testing.T) {
	const id = "claude-a.json"
	store := quota.NewStore()
	put := func(at time.Time, weekly model.Window) model.AuthSnapshot {
		store.Put(model.AuthSnapshot{AuthID: id, ObservedAt: at, Source: model.SourceUsageEndpoint, Windows: []model.Window{
			{Kind: model.WindowSession, Utilization: 0.1, ResetsAt: testNow.Add(2 * time.Hour)},
			weekly,
		}})
		snap, _ := store.Get(id)
		return snap
	}
	refusedAt := testNow.Add(-time.Hour)
	full := model.Window{Kind: model.WindowWeekly, Utilization: 1, ResetsAt: weeklyReset, Status: model.StatusRejected}
	put(refusedAt, full)
	store.RecordRefusals(id, []model.Window{full}, refusedAt, refusedAt)
	entry := heldEntry(weeklyReset.Add(20 * time.Second))
	open := model.Window{Kind: model.WindowWeekly, Utilization: 0, ResetsAt: weeklyReset}

	firstAt := testNow.Add(-2 * time.Minute)
	if _, ok := findStaleHold(entry, put(firstAt, open), store.History(id, 1, firstAt), firstAt); ok {
		t.Fatal("one low read found a stale cooldown; a clearing takes two")
	}
	hold, ok := findStaleHold(entry, put(testNow, open), store.History(id, 1, testNow), testNow)
	if !ok {
		t.Fatalf("the confirming read found no stale cooldown; history = %+v", store.History(id, 1, testNow))
	}
	if !hold.released.Equal(firstAt.Truncate(time.Second)) || hold.span != refusedAt.Unix() {
		t.Errorf("hold = %+v, want released at the first low read and keyed by the refusal", hold)
	}
}

// staleHoldFixture is pollFixture with claude-a.json cooled by the host until
// the weekly reset, after a refusal span the provider cleared an hour ago.
func staleHoldFixture(t *testing.T, tp *testPlugin) {
	t.Helper()
	pollFixture(t, tp)
	tp.host.files[0].Unavailable = true
	tp.host.files[0].NextRetryAfter = weeklyReset.Add(20 * time.Second)
	tp.quota.ImportHistory(map[string][]model.WindowHistory{
		"claude-a.json": {{
			Kind: model.WindowWeekly,
			Cycles: []model.Cycle{
				{ResetsAt: weeklyReset, Samples: []model.Sample{{At: testNow.Add(-4 * time.Hour), Utilization: 1}}},
				{ResetsAt: weeklyReset, Samples: []model.Sample{{At: testNow.Add(-time.Hour), Utilization: 0.02}}},
			},
			Locks: []model.Lock{ended(testNow.Add(-3*time.Hour), testNow.Add(-time.Hour), model.LockEndCleared, weeklyReset)},
		}},
	}, testNow.Add(-5*time.Hour))
}

func holdWarnings(tp *testPlugin, now time.Time) []string {
	out := []string{}
	for _, w := range tp.Status(now, "").Warnings {
		if strings.Contains(w, "keeps it cooled") {
			out = append(out, w)
		}
	}
	return out
}

func TestPollClearsAStaleHostCooldownOnce(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)

	for range 2 {
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 {
		t.Fatalf("reset_cooldown called %d times, want once", n)
	}
	if !tp.host.sent(`{"auth_index":"idx-a"}`) {
		t.Error("the reset did not name the cooled credential's auth index")
	}
	if w := holdWarnings(tp, testNow); len(w) != 0 {
		t.Errorf("warnings = %v, want none once the host cleared the cooldown", w)
	}
}

func TestAStaleHoldTheHostKeepsIsNotResetAgain(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)
	tp.host.resetKeeps = true

	for range 3 {
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 {
		t.Fatalf("reset_cooldown called %d times, want once per refusal span", n)
	}
	w := holdWarnings(tp, testNow)
	if len(w) != 1 || !strings.Contains(w[0], "cooled it again") || !strings.Contains(w[0], "for 1h,") {
		t.Errorf("warnings = %v, want one naming the cooldown the host kept", w)
	}
}

// TestAHoldReimposedToTheSameDeadlineIsNotResetAgain covers a refusal the
// provider repeats after the plugin cleared the cooldown, ending again in a
// clearing: the host cools the seat to the same reset, and the plugin leaves
// that cooldown to the operator rather than spend another request on it.
func TestAHoldReimposedToTheSameDeadlineIsNotResetAgain(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 {
		t.Fatalf("reset_cooldown called %d times, want once", n)
	}

	refusedAt := testNow.Add(10 * time.Minute)
	full := model.Window{Kind: model.WindowWeekly, Utilization: 1, ResetsAt: weeklyReset, Status: model.StatusRejected}
	tp.quota.MergeHeaders("claude-a.json", []model.Window{full}, refusedAt)
	tp.quota.RecordRefusals("claude-a.json", []model.Window{full}, refusedAt, refusedAt)
	tp.host.mu.Lock()
	tp.host.files[0].Unavailable = true
	tp.host.files[0].NextRetryAfter = weeklyReset.Add(5 * time.Second)
	tp.host.mu.Unlock()
	clock := refusedAt
	for range 3 {
		clock = clock.Add(5 * time.Minute)
		tp.now = func() time.Time { return clock }
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 {
		t.Fatalf("reset_cooldown called %d times, want the first reset only", n)
	}
	w := holdWarnings(tp, clock)
	if len(w) != 1 || !strings.Contains(w[0], "cooled it again") {
		t.Errorf("warnings = %v, want one naming the cooldown the host put back", w)
	}
}

// TestASecondEarlyResetInTheSameWeekIsAskedAgain covers a seat the plugin
// cleared that ran out again days later, before the same weekly reset, and
// had its quota given back once more.
func TestASecondEarlyResetInTheSameWeekIsAskedAgain(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	refusedAt := testNow.Add(48 * time.Hour)
	full := model.Window{Kind: model.WindowWeekly, Utilization: 1, ResetsAt: weeklyReset, Status: model.StatusRejected}
	tp.quota.MergeHeaders("claude-a.json", []model.Window{full}, refusedAt)
	tp.quota.RecordRefusals("claude-a.json", []model.Window{full}, refusedAt, refusedAt)
	tp.host.mu.Lock()
	tp.host.files[0].Unavailable = true
	tp.host.files[0].NextRetryAfter = weeklyReset.Add(5 * time.Second)
	tp.host.mu.Unlock()
	clock := refusedAt.Add(20 * time.Hour)
	for range 2 {
		clock = clock.Add(5 * time.Minute)
		tp.now = func() time.Time { return clock }
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 2 {
		t.Fatalf("reset_cooldown called %d times, want once for each early reset", n)
	}
	if w := holdWarnings(tp, clock); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}

func TestAnOlderHostIsAskedOnceAndWarned(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)
	tp.host.resetErr = errors.New("unsupported host callback " + MethodHostRoutingResetCooldown)

	for range 2 {
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 {
		t.Fatalf("reset_cooldown called %d times, want once per process", n)
	}
	w := holdWarnings(tp, testNow)
	if len(w) != 1 || !strings.Contains(w[0], "8.0.12") || !strings.Contains(w[0], "reset-quota") {
		t.Errorf("warnings = %v, want one naming the host version and the manual reset", w)
	}
}

func TestAFailedResetIsWarnedAndNotRetried(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	staleHoldFixture(t, tp)
	tp.host.resetErr = errors.New("auth not found for auth_index idx-a")

	for range 2 {
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if n := tp.host.count(MethodHostRoutingResetCooldown); n != 1 {
		t.Fatalf("reset_cooldown called %d times, want once", n)
	}
	w := holdWarnings(tp, testNow)
	if len(w) != 1 || !strings.Contains(w[0], "clearing the cooldown failed") {
		t.Errorf("warnings = %v, want one carrying the failure", w)
	}
}

func TestDurationText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:               "20s",
		4*time.Minute + 59*time.Second: "4m",
		time.Hour:                      "1h",
		3*time.Hour + 5*time.Minute:    "3h 5m",
		65*time.Hour + 48*time.Minute:  "2d 17h",
		72 * time.Hour:                 "3d",
	} {
		if got := durationText(d); got != want {
			t.Errorf("durationText(%v) = %q, want %q", d, got, want)
		}
	}
}
