package runtime

import (
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
	for _, tc := range []struct {
		auth, span string
		want       time.Time
		err        error
	}{
		{"seat-a", model.BoostHour, testNow.Add(time.Hour), nil},
		{"seat-a", model.BoostFourHours, testNow.Add(4 * time.Hour), nil},
		{"seat-a", model.BoostUntilCleared, time.Time{}, nil},
		{"seat-b", model.BoostReset, at(t, "2026-09-05T06:00:00Z"), nil},
		// No all-models weekly window: the 5-hour reset.
		{"seat-session-only", model.BoostReset, at(t, "2026-09-05T01:00:00Z"), nil},
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

// Every configure reads the boosts file again, so a build the host keeps or
// rolls back to takes the changes another build made.
func TestConfigureRereadsTheBoostsFile(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	if _, err := tp.Boost("seat-a", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}
	other := seededPlugin(t, tp.opts.HistoryFile, testConfigYAML)
	other.Unboost("seat-a", testNow)
	if _, err := other.Boost("seat-b", model.BoostUntilCleared, testNow); err != nil {
		t.Fatal(err)
	}

	tp.register(t, MethodPluginReconfigure, testConfigYAML)
	if _, ok := tp.boosts.active("seat-a", testNow); ok {
		t.Error("the reconfigured build kept seat-a's cleared boost")
	}
	if _, ok := tp.boosts.active("seat-b", testNow); !ok {
		t.Error("the reconfigured build missed seat-b's boost")
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
