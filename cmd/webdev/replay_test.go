package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

var replayAt = time.Date(2026, 9, 23, 14, 55, 0, 0, time.UTC)

func sample(ago time.Duration, u float64) model.Sample {
	return model.Sample{At: replayAt.Add(-ago), Utilization: u}
}

func TestBefore(t *testing.T) {
	t.Parallel()
	hs := []model.WindowHistory{{
		Kind: model.WindowSession,
		Cycles: []model.Cycle{
			{ResetsAt: replayAt.Add(-time.Hour), Samples: []model.Sample{sample(3*time.Hour, 0.2)}},
			{ResetsAt: replayAt.Add(4 * time.Hour), Samples: []model.Sample{sample(time.Hour, 0.1), sample(0, 0.3), sample(-time.Minute, 0.4)}},
			{ResetsAt: replayAt.Add(9 * time.Hour), Samples: []model.Sample{sample(-6*time.Hour, 0.1)}},
		},
	}}
	u := func(d time.Duration) int64 { return replayAt.Add(d).Unix() }
	hs[0].Locks = []model.Lock{
		{From: u(-2 * time.Hour), To: u(-time.Hour), End: model.LockEndReset},
		{From: u(-time.Minute), To: u(time.Hour), End: model.LockEndServed},
		{From: u(time.Minute), To: u(time.Hour), End: model.LockEndServed},
	}
	got := before(hs, replayAt)
	want := []model.WindowHistory{{
		Kind: model.WindowSession,
		Cycles: []model.Cycle{
			{ResetsAt: replayAt.Add(-time.Hour), Samples: []model.Sample{sample(3*time.Hour, 0.2)}},
			{ResetsAt: replayAt.Add(4 * time.Hour), Samples: []model.Sample{sample(time.Hour, 0.1), sample(0, 0.3)}},
		},
		Locks: []model.Lock{hs[0].Locks[0], {From: u(-time.Minute), To: u(4 * time.Hour)}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("before kept\n %+v\nwant\n %+v", got, want)
	}
	if n, m, end := len(hs[0].Cycles), len(hs[0].Locks), hs[0].Locks[1].End; n != 3 || m != 3 || end != model.LockEndServed {
		t.Errorf("before changed its input: %d cycles, %d locks and a span ended %q, want 3, 3 and served", n, m, end)
	}
}

func TestPresentWindow(t *testing.T) {
	t.Parallel()
	session := func(reset time.Time) model.WindowHistory {
		return model.WindowHistory{Kind: model.WindowSession, Cycles: []model.Cycle{{
			ResetsAt: reset, Samples: []model.Sample{sample(time.Hour, 0.2), sample(10*time.Minute, 0.65)},
		}}}
	}
	locked := func(h model.WindowHistory, from, to time.Duration) model.WindowHistory {
		h.Locks = []model.Lock{{From: replayAt.Add(-from).Unix(), To: replayAt.Add(-to).Unix()}}
		return h
	}
	cases := []struct {
		name     string
		h        model.WindowHistory
		ok       bool
		util     float64
		reset    time.Time
		rejected bool
	}{
		{name: "open cycle", h: session(replayAt.Add(2 * time.Hour)), ok: true, util: 0.65, reset: replayAt.Add(2 * time.Hour)},
		{name: "reset passed rolls to an empty window", h: session(replayAt.Add(-time.Hour)), ok: true, util: 0,
			reset: replayAt.Add(-time.Hour + model.SessionDuration)},
		{name: "lock covering now", h: locked(session(replayAt.Add(2*time.Hour)), 5*time.Minute, -time.Hour),
			ok: true, util: 0.65, reset: replayAt.Add(2 * time.Hour), rejected: true},
		{name: "lock already over", h: locked(session(replayAt.Add(2*time.Hour)), time.Hour, 30*time.Minute),
			ok: true, util: 0.65, reset: replayAt.Add(2 * time.Hour)},
		{name: "no cycle with samples", h: model.WindowHistory{Kind: model.WindowSession}, ok: false},
	}
	for _, c := range cases {
		w, ok := presentWindow(c.h, replayAt)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if w.Utilization != c.util || !w.ResetsAt.Equal(c.reset) {
			t.Errorf("%s: utilization %v resets %v, want %v and %v", c.name, w.Utilization, w.ResetsAt, c.util, c.reset)
		}
		if rejected := w.Status == model.StatusRejected; rejected != c.rejected {
			t.Errorf("%s: rejected = %v, want %v", c.name, rejected, c.rejected)
		}
	}
}

func TestLocksOf(t *testing.T) {
	t.Parallel()
	spans := []lockSpan{
		{Kind: model.WindowWeekly, From: 100, To: 200},
		{Kind: model.WindowSession, From: 50, To: 90, CutBySuccess: true},
		{Kind: model.WindowWeekly, From: 300, To: 400, CutBySuccess: true},
		{Kind: model.WindowWeekly, From: 500, To: 500},
		{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, From: 10, To: 20},
	}
	cases := []struct {
		kind  model.WindowKind
		scope string
		want  []model.Lock
	}{
		{kind: model.WindowWeekly, want: []model.Lock{{From: 100, To: 200, End: model.LockEndReset}, {From: 300, To: 400, End: model.LockEndServed}}},
		{kind: model.WindowSession, want: []model.Lock{{From: 50, To: 90, End: model.LockEndServed}}},
		{kind: model.WindowWeeklyScoped, scope: model.FamilyFable, want: []model.Lock{{From: 10, To: 20, End: model.LockEndReset}}},
		{kind: model.WindowWeeklyScoped, scope: "other", want: nil},
	}
	for _, c := range cases {
		if got := locksOf(spans, c.kind, c.scope); !reflect.DeepEqual(got, c.want) {
			t.Errorf("locksOf(%s, %q) = %v, want %v", c.kind, c.scope, got, c.want)
		}
	}
}

// TestMergeLocks covers the two sources joined: oldest first, and of two spans
// with the same ends the first source's.
func TestMergeLocks(t *testing.T) {
	t.Parallel()
	a := []model.Lock{{From: 300, To: 400, End: model.LockEndServed}, {From: 100, To: 200, End: model.LockEndCleared}}
	b := []model.Lock{{From: 100, To: 200, End: model.LockEndReset}, {From: 150, To: 250, End: model.LockEndReset}}
	want := []model.Lock{
		{From: 100, To: 200, End: model.LockEndCleared}, {From: 150, To: 250, End: model.LockEndReset},
		{From: 300, To: 400, End: model.LockEndServed},
	}
	if got := mergeLocks(a, b); !reflect.DeepEqual(got, want) {
		t.Errorf("mergeLocks = %v, want %v", got, want)
	}
}

// TestReplayKeepsTheFileLocks covers the spans a replay publishes: the
// history file's own, those the locks file adds, each once and with what
// ended it, and none that begins after the replayed instant; a span running
// at that instant is ongoing to its window's reset, and a span of either
// source covering it marks the window refused.
func TestReplayKeepsTheFileLocks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	u := func(d time.Duration) int64 { return replayAt.Add(d).Unix() }
	history := map[string]any{"version": 1, "seats": map[string][]model.WindowHistory{
		"claude-a.json": {{
			Kind: model.WindowWeekly,
			Cycles: []model.Cycle{{ResetsAt: replayAt.Add(24 * time.Hour), Samples: []model.Sample{
				sample(3*time.Hour, 0.9), sample(2*time.Hour, 1.0),
			}}},
			Locks: []model.Lock{{From: u(-2 * time.Hour), To: u(-time.Hour), End: model.LockEndCleared}, {From: u(time.Hour), To: u(2 * time.Hour), End: model.LockEndServed}},
		}, {
			Kind: model.WindowSession,
			Cycles: []model.Cycle{{ResetsAt: replayAt.Add(2 * time.Hour), Samples: []model.Sample{
				sample(3*time.Hour, 0.2), sample(time.Hour, 1.0),
			}}},
		}},
		"claude-b.json": {{
			Kind: model.WindowSession,
			Cycles: []model.Cycle{{ResetsAt: replayAt.Add(-10 * time.Minute), Samples: []model.Sample{
				sample(3*time.Hour, 0.3), sample(20*time.Minute, 1.0),
			}}},
		}},
		"claude-c.json": {{
			Kind: model.WindowSession,
			Cycles: []model.Cycle{{ResetsAt: replayAt.Add(4 * time.Hour), Samples: []model.Sample{
				sample(-time.Hour, 0.1),
			}}},
		}},
	}}
	extra := map[string][]lockSpan{
		"claude-a.json": {
			{Kind: model.WindowWeekly, From: u(-2 * time.Hour), To: u(-time.Hour)},
			{Kind: model.WindowWeekly, From: u(-30 * time.Minute), To: u(24 * time.Hour)},
			{Kind: model.WindowSession, From: u(-time.Hour), To: u(time.Hour), CutBySuccess: true},
		},
		"claude-b.json": {{Kind: model.WindowSession, From: u(-30 * time.Minute), To: u(time.Hour)}},
		"claude-c.json": {{Kind: model.WindowSession, From: u(-30 * time.Minute), To: u(10 * time.Minute)}},
	}
	historyPath, locksPath := filepath.Join(dir, "history.json"), filepath.Join(dir, "locks.json")
	for path, v := range map[string]any{historyPath: history, locksPath: extra} {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	f := &fixture{}
	if err := f.replay(historyPath, locksPath, replayAt); err != nil {
		t.Fatal(err)
	}
	want := map[model.WindowKind][]model.Lock{
		model.WindowWeekly: {
			{From: u(-2 * time.Hour), To: u(-time.Hour), End: model.LockEndCleared},
			{From: u(-30 * time.Minute), To: u(24 * time.Hour)},
		},
		model.WindowSession: {{From: u(-time.Hour), To: u(2 * time.Hour)}},
	}
	for _, h := range f.replayed["claude-a.json"] {
		if !reflect.DeepEqual(h.Locks, want[h.Kind]) {
			t.Errorf("%s locks = %v, want %v", h.Kind, h.Locks, want[h.Kind])
		}
	}
	// A span running at the replayed instant stays ongoing where the window's
	// newest cycle ended before it, and runs a window's length from its start
	// where the window has no cycle by then.
	for id, want := range map[string][]model.Lock{
		"claude-b.json": {{From: u(-30 * time.Minute), To: u(-10*time.Minute + model.SessionDuration)}},
		"claude-c.json": {{From: u(-30 * time.Minute), To: u(-30*time.Minute + model.SessionDuration)}},
	} {
		for _, h := range f.replayed[id] {
			if !reflect.DeepEqual(h.Locks, want) {
				t.Errorf("%s %s locks = %v, want %v", id, h.Kind, h.Locks, want)
			}
		}
	}
	if n := len(f.replayed["claude-b.json"]) + len(f.replayed["claude-c.json"]); n != 2 {
		t.Errorf("seats b and c publish %d windows, want one each", n)
	}
	for _, w := range f.snapshots["claude-a.json"].Windows {
		if !w.Blocking() {
			t.Errorf("%s is not refused at the replayed instant", w.Kind)
		}
	}

	fileOnly := &fixture{}
	if err := fileOnly.replay(historyPath, "", replayAt.Add(-90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, w := range fileOnly.snapshots["claude-a.json"].Windows {
		if refused := w.Blocking(); refused != (w.Kind == model.WindowWeekly) {
			t.Errorf("%s refused = %v without -locks, want only the weekly window the file refuses", w.Kind, refused)
		}
	}
}
