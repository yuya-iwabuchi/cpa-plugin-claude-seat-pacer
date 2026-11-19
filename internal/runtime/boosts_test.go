package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// A boost sends a new conversation to its seat ahead of the pace winner and
// says so in the decision, and leaves a conversation bound elsewhere where it
// is.
func TestColdPickPrefersABoostedSeat(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	if _, err := tp.Boost("seat-a", model.BoostHour, testNow); err != nil {
		t.Fatal(err)
	}

	resp := tp.pick(t, pickRequest(fableModel, "k-new", "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want the boosted seat-a over the pace winner seat-b", resp)
	}
	d := tp.lastDecision(t)
	if !strings.Contains(d.Note, "boosted seat") || len(d.Scores) != 2 || !d.Scores[0].Boosted || d.Scores[0].AuthID != "seat-a" {
		t.Errorf("decision = %+v, want seat-a ranked first, boosted, and the note saying so", d)
	}

	tp.bindings.Bind("claude", fableModel, "k-bound", "seat-b", testNow)
	if resp := tp.pick(t, pickRequest(fableModel, "k-bound", "seat-a", "seat-b")); resp.AuthID != "seat-b" {
		t.Fatalf("bound response = %+v, want the conversation kept on seat-b", resp)
	}
}

// A boost on a full seat routes nothing to it: the gate comes first.
func TestBoostNeverMakesAFullSeatEligible(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	full := seatA(t)
	full.Windows[1].Utilization = 1
	tp.quota.Put(full)
	if _, err := tp.Boost("seat-a", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}
	if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b")); resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want seat-b: the boosted seat-a is full", resp)
	}
}

func TestBoostSpans(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	tp.quota.Put(seat("seat-session-only", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0.6, ResetsAt: at(t, "2026-09-05T01:00:00Z"), Duration: model.SessionDuration}))
	tp.quota.Put(seat("seat-family-weekly", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0.6, ResetsAt: at(t, "2026-09-05T01:00:00Z"), Duration: model.SessionDuration},
		model.Window{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0.2, ResetsAt: at(t, "2026-09-08T06:00:00Z"), Duration: model.WeeklyDuration},
		model.Window{Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus, Utilization: 0.2, ResetsAt: at(t, "2026-09-07T06:00:00Z"), Duration: model.WeeklyDuration}))
	tp.quota.Put(seat("seat-weekly-rolled", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0.6, ResetsAt: at(t, "2026-09-05T01:00:00Z"), Duration: model.SessionDuration},
		model.Window{Kind: model.WindowWeekly, Utilization: 0.9, ResetsAt: at(t, "2026-09-04T21:30:00Z"), Duration: model.WeeklyDuration}))
	tp.quota.Put(seat("seat-weekly-unopened", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0.6, ResetsAt: at(t, "2026-09-05T01:00:00Z"), Duration: model.SessionDuration},
		model.Window{Kind: model.WindowWeekly, Duration: model.WeeklyDuration}))
	for _, tc := range []struct {
		auth, span string
		want       time.Time
		err        error
	}{
		{"seat-a", model.BoostHour, testNow.Add(time.Hour), nil},
		{"seat-a", model.BoostFourHours, testNow.Add(4 * time.Hour), nil},
		{"seat-a", model.BoostUntilCleared, time.Time{}, nil},
		{"seat-b", model.BoostReset, at(t, "2026-09-05T06:00:00Z"), nil},
		// No weekly window: the 5-hour reset.
		{"seat-session-only", model.BoostReset, at(t, "2026-09-05T01:00:00Z"), nil},
		// Family weekly windows only: the soonest of their resets.
		{"seat-family-weekly", model.BoostReset, at(t, "2026-09-07T06:00:00Z"), nil},
		// A weekly reset the reading names as passed: the one a week on.
		{"seat-weekly-rolled", model.BoostReset, at(t, "2026-09-11T21:30:00Z"), nil},
		// A weekly window that names no reset: the 5-hour reset.
		{"seat-weekly-unopened", model.BoostReset, at(t, "2026-09-05T01:00:00Z"), nil},
		{"seat-unread", model.BoostReset, time.Time{}, ErrNoReset},
		{"seat-a", "2d", time.Time{}, ErrUnknownSpan},
	} {
		b, err := tp.Boost(tc.auth, tc.span, testNow)
		if !errors.Is(err, tc.err) || !b.Until.Equal(tc.want) {
			t.Errorf("Boost(%s, %s) = until %v err %v, want until %v err %v", tc.auth, tc.span, b.Until, err, tc.want, tc.err)
		}
	}
}

// A boost lasts its span and no longer, and clearing one ends it.
func TestBoostEnds(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	if _, err := tp.Boost("seat-a", model.BoostHour, testNow); err != nil {
		t.Fatal(err)
	}
	if row := statusRow(t, tp.Status(testNow, fableModel), "seat-a"); row.Boost == nil || !row.Score.Boosted {
		t.Fatalf("status row = %+v, want seat-a boosted", row)
	}
	if row := statusRow(t, tp.Status(testNow.Add(time.Hour), fableModel), "seat-a"); row.Boost != nil || row.Score.Boosted {
		t.Fatalf("status row an hour on = %+v, want the boost ended", row)
	}

	if _, err := tp.Boost("seat-a", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}
	if !tp.Unboost("seat-a", testNow) {
		t.Fatal("Unboost reported no boost on seat-a")
	}
	if row := statusRow(t, tp.Status(testNow, fableModel), "seat-a"); row.Boost != nil {
		t.Fatalf("status row = %+v, want the boost cleared", row)
	}
}

// The boosts file carries every live boost to the next build, and goes once
// none is live.
func TestBoostsSurviveAReload(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	if _, err := tp.Boost("seat-a", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := tp.Boost("seat-b", model.BoostHour, testNow.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(tp.opts.HistoryFile), boostsFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("boosts file: %v", err)
	}

	next := seededPlugin(t, tp.opts.HistoryFile, testConfigYAML)
	if _, ok := next.boosts.active("seat-a", testNow); !ok {
		t.Error("the next build lost seat-a's boost")
	}
	if _, ok := next.boosts.active("seat-b", testNow); ok {
		t.Error("the next build restored seat-b's ended boost")
	}

	next.Unboost("seat-a", testNow)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("boosts file with nothing live: %v, want it removed", err)
	}
}

// A boost change whose save failed outlasts a reconfigure, which keeps the
// table rather than taking the older file's back, and the next change saves
// it.
func TestConfigureKeepsAnUnsavedBoostTable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through the directory's mode")
	}
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	for _, id := range []string{"seat-a", "seat-b"} {
		if _, err := tp.Boost(id, model.BoostUntilCleared, testNow); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(tp.opts.HistoryFile)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	tp.Unboost("seat-a", testNow)
	if _, err := tp.Boost("seat-session-only", model.BoostHour, testNow); err != nil {
		t.Fatal(err)
	}
	tp.register(t, MethodPluginReconfigure, testConfigYAML)
	if _, ok := tp.boosts.active("seat-a", testNow); ok {
		t.Error("a configure brought back the cleared boost on seat-a")
	}
	if _, ok := tp.boosts.active("seat-session-only", testNow); !ok {
		t.Error("a configure dropped the boost on seat-session-only")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := tp.Boost("seat-b", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}
	next := seededPlugin(t, tp.opts.HistoryFile, testConfigYAML)
	for id, want := range map[string]bool{"seat-a": false, "seat-b": true, "seat-session-only": true} {
		if _, ok := next.boosts.active(id, testNow); ok != want {
			t.Errorf("boosts file after the next change: %s boosted %v, want %v", id, ok, want)
		}
	}
}

// A build the host loads again, reusing its memory, takes at its register the
// boosts file another build wrote since, over a change of its own whose save
// failed.
func TestReusedBuildTakesANewerBoostsFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through the directory's mode")
	}
	a := newTestPlugin(t, testConfigYAML)
	a.seats(t)
	for _, id := range []string{"seat-a", "seat-b"} {
		if _, err := a.Boost(id, model.BoostUntilCleared, testNow); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(a.opts.HistoryFile)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	a.Unboost("seat-a", testNow)
	a.Call(MethodPluginQuiesce, nil)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	b := seededPlugin(t, a.opts.HistoryFile, testConfigYAML)
	if _, err := b.Boost("seat-session-only", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}
	b.Unboost("seat-b", testNow)
	b.Call(MethodPluginQuiesce, nil)

	a.register(t, MethodPluginRegister, testConfigYAML)
	want := map[string]bool{"seat-a": true, "seat-b": false, "seat-session-only": true}
	for id, on := range want {
		if _, ok := a.boosts.active(id, testNow); ok != on {
			t.Errorf("reused build: %s boosted %v, want %v as the other build left it", id, ok, on)
		}
	}
	next := seededPlugin(t, a.opts.HistoryFile, testConfigYAML)
	for id, on := range want {
		if _, ok := next.boosts.active(id, testNow); ok != on {
			t.Errorf("boosts file: %s boosted %v, want %v as the other build left it", id, ok, on)
		}
	}
}

// A boost change whose save failed is written once the directory allows it
// by the next configure, poll tick, quiesce or shutdown, so the next build
// restores the change.
func TestAFailedBoostSaveIsRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through the directory's mode")
	}
	for _, tc := range []struct {
		name  string
		retry func(t *testing.T, tp *testPlugin)
	}{
		{"reconfigure", func(t *testing.T, tp *testPlugin) {
			tp.register(t, MethodPluginReconfigure, testConfigYAML)
		}},
		{"poll", func(t *testing.T, tp *testPlugin) { tp.guardedPoll(context.Background()) }},
		{"quiesce", func(t *testing.T, tp *testPlugin) { tp.callOK(t, MethodPluginQuiesce, nil, nil) }},
		{"shutdown", func(t *testing.T, tp *testPlugin) { tp.Shutdown() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, testConfigYAML)
			tp.seats(t)
			for _, id := range []string{"seat-a", "seat-b"} {
				if _, err := tp.Boost(id, model.BoostUntilCleared, testNow); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Dir(tp.opts.HistoryFile)
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			tp.Unboost("seat-a", testNow)
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}

			tc.retry(t, tp)
			next := seededPlugin(t, tp.opts.HistoryFile, testConfigYAML)
			for id, want := range map[string]bool{"seat-a": false, "seat-b": true} {
				if _, ok := next.boosts.active(id, testNow); ok != want {
					t.Errorf("build after the %s: %s boosted %v, want %v", tc.name, id, ok, want)
				}
			}
		})
	}
}

// A boost outlasts one listing that leaves its seat out, as the seat's
// history does, and goes at the second in a row.
func TestBoostOutlastsOneMissedListing(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	if _, err := tp.Boost("seat-a", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}
	only := func(ids ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			m[id] = struct{}{}
		}
		return m
	}
	for i, keep := range []map[string]struct{}{only("seat-b"), only("seat-a", "seat-b"), only("seat-b")} {
		tp.pruneBoosts(keep, testNow)
		if _, ok := tp.boosts.active("seat-a", testNow); !ok {
			t.Fatalf("listing %d dropped seat-a's boost after a single miss", i)
		}
	}
	tp.pruneBoosts(only("seat-b"), testNow)
	if _, ok := tp.boosts.active("seat-a", testNow); ok {
		t.Error("seat-a kept its boost through two listings in a row without it")
	}
	if next := seededPlugin(t, tp.opts.HistoryFile, testConfigYAML); len(next.boosts.live(testNow)) != 0 {
		t.Error("the boosts file still holds the dropped boost")
	}
}

// A boosted seat with a weekly cap takes a new conversation ahead of an
// eligible seat with none.
func TestBoostedCappedSeatBeatsASessionOnlySeat(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	tp.quota.Put(seat("seat-session-only", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0.1, ResetsAt: at(t, "2026-09-05T01:00:00Z"), Duration: model.SessionDuration}))
	if resp := tp.pick(t, pickRequest("claude-opus-5", "k1", "seat-a", "seat-session-only")); resp.AuthID != "seat-session-only" {
		t.Fatalf("unboosted response = %+v, want seat-session-only", resp)
	}
	if _, err := tp.Boost("seat-a", model.BoostHour, testNow); err != nil {
		t.Fatal(err)
	}
	if resp := tp.pick(t, pickRequest("claude-opus-5", "k2", "seat-a", "seat-session-only")); resp.AuthID != "seat-a" {
		t.Fatalf("boosted response = %+v, want the boosted seat-a", resp)
	}
}

func statusRow(t *testing.T, st model.Status, id string) model.AuthStatus {
	t.Helper()
	for _, row := range st.Auths {
		if row.AuthID == id {
			return row
		}
	}
	t.Fatalf("status has no row %s", id)
	return model.AuthStatus{}
}
