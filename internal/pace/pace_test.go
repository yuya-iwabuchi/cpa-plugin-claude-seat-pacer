package pace

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

const tolerance = 1e-9

// testNow is the instant tests score at unless they need a second one to show
// a window moving.
var testNow = time.Date(2026, 9, 4, 19, 45, 0, 0, time.UTC)

func at(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse %q: %v", ts, err)
	}
	return parsed
}

// duration is the window length a kind implies.
func duration(kind model.WindowKind) time.Duration {
	if kind == model.WindowSession {
		return model.SessionDuration
	}
	return model.WeeklyDuration
}

// window is a reading stated the way the usage endpoint states one: a
// utilization and a reset instant.
func window(kind model.WindowKind, scope string, util float64, resetsAt time.Time) model.Window {
	return model.Window{
		Kind:        kind,
		Scope:       scope,
		Utilization: util,
		Duration:    duration(kind),
		ResetsAt:    resetsAt,
	}
}

// paced places a window on the clock so that Elapsed(now) is the requested
// fraction, which lets a test state the pace position it cares about instead of
// a reset instant.
func paced(kind model.WindowKind, scope string, now time.Time, elapsed, util float64) model.Window {
	d := duration(kind)
	return window(kind, scope, util, now.Add(time.Duration(float64(d)*(1-elapsed))))
}

func weekly(now time.Time, elapsed, util float64) model.Window {
	return paced(model.WindowWeekly, "", now, elapsed, util)
}

func seat(id string, now time.Time, windows ...model.Window) model.AuthSnapshot {
	return model.AuthSnapshot{
		AuthID:     id,
		Windows:    windows,
		ObservedAt: now,
		Source:     model.SourceUsageEndpoint,
	}
}

func snapshots(snaps ...model.AuthSnapshot) map[string]model.AuthSnapshot {
	m := make(map[string]model.AuthSnapshot, len(snaps))
	for _, s := range snaps {
		m[s.AuthID] = s
	}
	return m
}

func rankedIDs(scores []model.Score) []string {
	out := make([]string, 0, len(scores))
	for _, s := range scores {
		out = append(out, s.AuthID)
	}
	return out
}

func scoreOf(t *testing.T, scores []model.Score, id string) model.Score {
	t.Helper()
	for _, s := range scores {
		if s.AuthID == id {
			return s
		}
	}
	t.Fatalf("no score for %q in %v", id, rankedIDs(scores))
	return model.Score{}
}

func windowScoreOf(t *testing.T, s model.Score, kind model.WindowKind, scope string) model.WindowScore {
	t.Helper()
	for _, ws := range s.Windows {
		if ws.Kind == kind && ws.Scope == scope {
			return ws
		}
	}
	t.Fatalf("no %s/%q window in score for %q", kind, scope, s.AuthID)
	return model.WindowScore{}
}

func TestTarget(t *testing.T) {
	linear := model.Defaults().Pace

	steep := linear
	steep.CurveExponent = 2

	shallow := linear
	shallow.CurveExponent = 0.5

	cautious := linear
	cautious.LandingTarget = 0.9

	cases := []struct {
		name    string
		cfg     model.PaceConfig
		elapsed float64
		want    float64
	}{
		// A linear curve landing at 1.0 makes target and elapsed the same
		// number, which is the reference every other curve bends away from.
		{"linear start", linear, 0, 0},
		{"linear half", linear, 0.5, 0.5},
		{"linear nine tenths", linear, 0.9, 0.9},
		{"linear end", linear, 1, 1},
		{"clamps below zero", linear, -0.5, 0},
		{"clamps above one", linear, 1.5, 1},
		{"exponent above one lands late", steep, 0.5, 0.25},
		{"exponent below one lands early", shallow, 0.25, 0.5},
		{"landing target scales the curve", cautious, 0.5, 0.45},
		{"landing target caps the end", cautious, 1, 0.9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Target(tc.cfg, tc.elapsed)
			if math.Abs(got-tc.want) > tolerance {
				t.Fatalf("Target(elapsed=%v) = %v, want %v", tc.elapsed, got, tc.want)
			}
		})
	}
}

// A window at the end of its life targets full utilization, so its slack is
// whatever headroom is left. No end-of-window special case is needed: a nearly
// reset window with room outranks a freshly reset one that has none.
func TestSlackApproachesHeadroomAsWindowCloses(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	closing := seat("closing", now, weekly(now, 0.9999, 0.35))
	fresh := seat("fresh", now, weekly(now, 0, 0))

	closingScore := ScoreAuth(cfg, closing, "claude-opus-5", now)
	ws := windowScoreOf(t, closingScore, model.WindowWeekly, "")
	if want := 1 - 0.35; math.Abs(ws.Slack-want) > 1e-3 {
		t.Fatalf("slack at end of window = %v, want ~%v", ws.Slack, want)
	}

	ranked := Rank(cfg, snapshots(closing, fresh), []string{"closing", "fresh"}, "claude-opus-5", now)
	best, ok := Best(ranked)
	if !ok {
		t.Fatal("no eligible credential")
	}
	if best.AuthID != "closing" {
		t.Fatalf("winner = %q (totals %v), want the closing window with headroom",
			best.AuthID, rankedIDs(ranked))
	}
}

func TestScoreAuthBreakdownCoversEveryWindow(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	snap := seat("seat", now,
		paced(model.WindowSession, "", now, 0.5, 0.2),
		weekly(now, 0.25, 0.1),
		paced(model.WindowWeeklyScoped, model.FamilyOpus, now, 0.25, 0.6),
	)
	score := ScoreAuth(cfg, snap, "claude-fable-5-1", now)

	// A linear curve landing at 1.0 puts target on elapsed, so every number
	// below is arithmetic a reader can redo by hand.
	want := []model.WindowScore{
		{Kind: model.WindowSession, Elapsed: 0.5, Target: 0.5, Utilization: 0.2, Slack: 0.3, Weight: cfg.SessionWeight},
		{Kind: model.WindowWeekly, Elapsed: 0.25, Target: 0.25, Utilization: 0.1, Slack: 0.15, Weight: cfg.WeeklyWeight},
		// An Opus cap is invisible to a Fable request, so it is rendered but
		// carries no weight.
		{Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus, Elapsed: 0.25, Target: 0.25, Utilization: 0.6, Slack: -0.35},
	}
	if len(score.Windows) != len(want) {
		t.Fatalf("breakdown has %d windows, want %d", len(score.Windows), len(want))
	}
	for i, got := range score.Windows {
		w := want[i]
		if got.Kind != w.Kind || got.Scope != w.Scope {
			t.Fatalf("window %d = %s/%q, want %s/%q", i, got.Kind, got.Scope, w.Kind, w.Scope)
		}
		if !got.ResetsAt.Equal(snap.Windows[i].ResetsAt) {
			t.Fatalf("window %d ResetsAt = %v, want %v", i, got.ResetsAt, snap.Windows[i].ResetsAt)
		}
		for _, f := range []struct {
			name      string
			got, want float64
		}{
			{"Elapsed", got.Elapsed, w.Elapsed},
			{"Target", got.Target, w.Target},
			{"Utilization", got.Utilization, w.Utilization},
			{"Slack", got.Slack, w.Slack},
			{"Weight", got.Weight, w.Weight},
		} {
			if math.Abs(f.got-f.want) > tolerance {
				t.Fatalf("window %d %s = %v, want %v", i, f.name, f.got, f.want)
			}
		}
	}

	// 0.35*0.3 + 1.0*0.15 - 0.25*0.2, the busiest counted window being the
	// session one at 0.2.
	if wantTotal := 0.205; math.Abs(score.Total-wantTotal) > tolerance {
		t.Fatalf("Total = %v, want %v", score.Total, wantTotal)
	}
	if wantPenalty := cfg.RawWeight * 0.2; math.Abs(score.RawPenalty-wantPenalty) > tolerance {
		t.Fatalf("RawPenalty = %v, want %v", score.RawPenalty, wantPenalty)
	}
}

func TestScoreAuthEligibility(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	rejected := weekly(now, 0.5, 0.5)
	rejected.Status = model.StatusRejected

	critical := weekly(now, 0.5, 0.5)
	critical.Severity = model.SeverityCritical

	rejectedAndSpent := weekly(now, 0.5, 1.0)
	rejectedAndSpent.Status = model.StatusRejected

	warned := weekly(now, 0.5, 0.5)
	warned.Status = model.StatusAllowedWarning
	warned.Severity = model.SeverityWarning

	cases := []struct {
		name     string
		windows  []model.Window
		eligible bool
		reason   string
	}{
		{"healthy", []model.Window{weekly(now, 0.5, 0.2)}, true, model.ReasonEligible},
		{"warning is not blocking", []model.Window{warned}, true, model.ReasonEligible},
		{"provider rejected", []model.Window{rejected}, false, model.ReasonRejected},
		{"severity critical", []model.Window{critical}, false, model.ReasonRejected},
		{"at hard cutoff", []model.Window{weekly(now, 0.5, cfg.HardCutoff)}, false, model.ReasonHardCutoff},
		{"past full utilization", []model.Window{weekly(now, 0.5, 1.5)}, false, model.ReasonHardCutoff},
		{"rejection outranks cutoff", []model.Window{rejectedAndSpent}, false, model.ReasonRejected},
		{"no windows", nil, false, model.ReasonNoWindow},
		{
			"only another family's cap",
			[]model.Window{paced(model.WindowWeeklyScoped, model.FamilyOpus, now, 0.5, 0.2)},
			false,
			model.ReasonNoWindow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := ScoreAuth(cfg, seat("seat", now, tc.windows...), "claude-fable-5-1", now)
			if score.Eligible != tc.eligible || score.Reason != tc.reason {
				t.Fatalf("eligible=%v reason=%q, want eligible=%v reason=%q",
					score.Eligible, score.Reason, tc.eligible, tc.reason)
			}
			if math.IsNaN(score.Total) || math.IsInf(score.Total, 0) {
				t.Fatalf("total = %v, want a finite score", score.Total)
			}
		})
	}

	// Age is the caller's policy: scoring itself ignores ObservedAt.
	t.Run("snapshot age does not gate", func(t *testing.T) {
		stale := seat("seat", now.Add(-24*time.Hour), weekly(now, 0.5, 0.2))
		if score := ScoreAuth(cfg, stale, "claude-fable-5-1", now); !score.Eligible {
			t.Fatalf("eligible=false reason=%q, want scoring to ignore snapshot age", score.Reason)
		}
	})
}

// A missing snapshot and a snapshot that says nothing about this request are
// different operator problems, so they carry different reasons.
func TestNoSnapshotAndNoBearingWindowDiffer(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	snaps := snapshots(
		seat("empty", now),
		seat("other-family", now, paced(model.WindowWeeklyScoped, model.FamilyOpus, now, 0.5, 0.2)),
	)
	ranked := Rank(cfg, snaps, []string{"missing", "empty", "other-family"}, "claude-fable-5-1", now)

	for _, tc := range []struct{ id, reason string }{
		{"missing", model.ReasonNoSnapshot},
		{"empty", model.ReasonNoWindow},
		{"other-family", model.ReasonNoWindow},
	} {
		s := scoreOf(t, ranked, tc.id)
		if s.Eligible || s.Reason != tc.reason {
			t.Fatalf("%s: eligible=%v reason=%q, want eligible=false reason=%q",
				tc.id, s.Eligible, s.Reason, tc.reason)
		}
	}
}

// A window with no open period has no place on the curve, so it adds no slack
// and no weight. Its recorded state still gates: a credential the provider has
// refused is refused whether or not the plugin knows when the window turns
// over.
func TestUnopenedWindowScoresNothingAndStillGates(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow
	live := weekly(now, 0.5, 0.2)

	unopened := func(util float64) model.Window {
		return model.Window{Kind: model.WindowSession, Utilization: util, Duration: model.SessionDuration}
	}

	t.Run("contributes no slack, weight or raw penalty", func(t *testing.T) {
		withoutIdle := ScoreAuth(cfg, seat("seat", now, live), "claude-opus-5", now)
		withIdle := ScoreAuth(cfg, seat("seat", now, live, unopened(0.9)), "claude-opus-5", now)

		if withIdle.Total != withoutIdle.Total {
			t.Fatalf("total with an unopened window = %v, want %v", withIdle.Total, withoutIdle.Total)
		}
		if withIdle.RawPenalty != withoutIdle.RawPenalty {
			t.Fatalf("raw penalty = %v, want %v", withIdle.RawPenalty, withoutIdle.RawPenalty)
		}
		if !withIdle.Eligible {
			t.Fatalf("eligible=false reason=%q, want an unopened window under cutoff not to gate", withIdle.Reason)
		}
		if got := windowScoreOf(t, withIdle, model.WindowSession, "").Weight; got != 0 {
			t.Fatalf("unopened window weight = %v, want 0", got)
		}
	})

	t.Run("provider rejection gates", func(t *testing.T) {
		dead := unopened(0)
		dead.Status = model.StatusRejected

		score := ScoreAuth(cfg, seat("seat", now, live, dead), "claude-opus-5", now)
		if score.Eligible || score.Reason != model.ReasonRejected {
			t.Fatalf("eligible=%v reason=%q, want eligible=false reason=%q",
				score.Eligible, score.Reason, model.ReasonRejected)
		}
		if got := windowScoreOf(t, score, model.WindowSession, "").Weight; got != 0 {
			t.Fatalf("unopened window weight = %v, want 0", got)
		}
	})

	t.Run("hard cutoff gates", func(t *testing.T) {
		score := ScoreAuth(cfg, seat("seat", now, live, unopened(1.0)), "claude-opus-5", now)
		if score.Eligible || score.Reason != model.ReasonHardCutoff {
			t.Fatalf("eligible=%v reason=%q, want eligible=false reason=%q",
				score.Eligible, score.Reason, model.ReasonHardCutoff)
		}
	})

	t.Run("another family's unopened cap does not gate", func(t *testing.T) {
		opusCap := model.Window{
			Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus,
			Utilization: 1.0, Duration: model.WeeklyDuration,
		}
		if score := ScoreAuth(cfg, seat("seat", now, live, opusCap), "claude-fable-5-1", now); !score.Eligible {
			t.Fatalf("eligible=false reason=%q, want an Opus cap not to gate a Fable request", score.Reason)
		}
	})

	// A seat whose only reading is a rejection is out of the pool, not the
	// winner of a one-candidate field.
	t.Run("a rejected seat never wins", func(t *testing.T) {
		dead := unopened(0)
		dead.Status = model.StatusRejected

		snaps := snapshots(seat("a-rejected", now, dead), seat("b-healthy", now, live))
		ranked := Rank(cfg, snaps, []string{"a-rejected", "b-healthy"}, "claude-opus-5", now)

		if r := scoreOf(t, ranked, "a-rejected"); r.Eligible || r.Reason != model.ReasonRejected {
			t.Fatalf("a-rejected: eligible=%v reason=%q, want eligible=false reason=%q",
				r.Eligible, r.Reason, model.ReasonRejected)
		}
		best, ok := Best(ranked)
		if !ok || best.AuthID != "b-healthy" {
			t.Fatalf("winner = (%q, %v), want (b-healthy, true)", best.AuthID, ok)
		}
		if _, ok := Best(Rank(cfg, snaps, []string{"a-rejected"}, "claude-opus-5", now)); ok {
			t.Fatal("Best returned a provider-rejected credential from a one-candidate field")
		}
	})
}

// A utilization that is not a finite number cannot be placed against the curve
// or ordered against another seat's score.
func TestNonFiniteUtilizationIsIneligible(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	for _, tc := range []struct {
		name string
		util float64
	}{
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			score := ScoreAuth(cfg, seat("seat", now, weekly(now, 0.5, tc.util)), "claude-opus-5", now)
			if score.Eligible || score.Reason != model.ReasonBadReading {
				t.Fatalf("eligible=%v reason=%q, want eligible=false reason=%q",
					score.Eligible, score.Reason, model.ReasonBadReading)
			}
		})
	}

	t.Run("an unopened window's reading is still read", func(t *testing.T) {
		dead := model.Window{Kind: model.WindowSession, Utilization: math.NaN(), Duration: model.SessionDuration}
		score := ScoreAuth(cfg, seat("seat", now, weekly(now, 0.5, 0.2), dead), "claude-opus-5", now)
		if score.Eligible || score.Reason != model.ReasonBadReading {
			t.Fatalf("eligible=%v reason=%q, want eligible=false reason=%q",
				score.Eligible, score.Reason, model.ReasonBadReading)
		}
	})

	t.Run("another family's cap is not read", func(t *testing.T) {
		score := ScoreAuth(cfg, seat("seat", now,
			weekly(now, 0.5, 0.2),
			paced(model.WindowWeeklyScoped, model.FamilyOpus, now, 0.5, math.NaN()),
		), "claude-fable-5-1", now)
		if !score.Eligible {
			t.Fatalf("eligible=false reason=%q, want an Opus cap unread by a Fable request", score.Reason)
		}
	})
}

// NaN compares false against every number, so a comparator without a branch for
// it is not transitive and orders by whatever order the host offered.
func TestRankOrdersUnreadableTotalsLast(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	snaps := snapshots(
		seat("good-high", now, weekly(now, 0.9, 0.1)),
		seat("good-low", now, weekly(now, 0.5, 0.4)),
		// Sorts before the cut seat by id, after it by eligibility order.
		seat("nan-a", now, weekly(now, 0.5, math.NaN())),
		seat("nan-b", now, weekly(now, 0.5, math.NaN())),
		seat("y-cut", now, weekly(now, 0.5, 1.0)),
	)
	want := []string{"good-high", "good-low", "y-cut", "nan-a", "nan-b"}

	orders := [][]string{
		{"good-high", "good-low", "y-cut", "nan-a", "nan-b"},
		{"nan-a", "nan-b", "y-cut", "good-low", "good-high"},
		{"nan-b", "good-low", "y-cut", "nan-a", "good-high"},
		{"y-cut", "nan-a", "good-high", "nan-b", "good-low"},
	}
	for _, order := range orders {
		ranked := Rank(cfg, snaps, append([]string{}, order...), "claude-opus-5", now)
		if got := rankedIDs(ranked); !reflect.DeepEqual(got, want) {
			t.Fatalf("Rank(%v) = %v, want %v", order, got, want)
		}
	}

	ranked := Rank(cfg, snaps, orders[0], "claude-opus-5", now)
	best, ok := Best(ranked)
	if !ok || best.AuthID != "good-high" {
		t.Fatalf("winner = (%q, %v), want (good-high, true)", best.AuthID, ok)
	}
	// An unreadable incumbent yields rather than holding its binding forever.
	if !ShouldSwitch(cfg, scoreOf(t, ranked, "nan-a"), best) {
		t.Fatal("ShouldSwitch(unreadable incumbent, healthy challenger) = false, want true")
	}
	if ShouldSwitch(cfg, best, scoreOf(t, ranked, "nan-a")) {
		t.Fatal("ShouldSwitch(healthy incumbent, unreadable challenger) = true, want false")
	}
}

// Two credentials can both sit exactly on their curves while one is nearly
// spent and the other is idle. Pace slack alone calls that a tie; the raw term
// is what sends the request to the idle seat.
func TestRawPenaltySpreadsLoadBetweenSeatsOnPace(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	// Named so that an alphabetical tie-break would pick the loaded seat.
	loaded := seat("a-loaded", now, weekly(now, 0.8, 0.8))
	idle := seat("b-idle", now, weekly(now, 0.1, 0.1))

	for _, s := range []model.AuthSnapshot{loaded, idle} {
		score := ScoreAuth(cfg, s, "claude-opus-5", now)
		if slack := windowScoreOf(t, score, model.WindowWeekly, "").Slack; math.Abs(slack) > tolerance {
			t.Fatalf("%s slack = %v, want a seat exactly on pace", s.AuthID, slack)
		}
	}

	ranked := Rank(cfg, snapshots(loaded, idle), []string{"a-loaded", "b-idle"}, "claude-opus-5", now)
	best, ok := Best(ranked)
	if !ok {
		t.Fatal("no eligible credential")
	}
	if best.AuthID != "b-idle" {
		t.Fatalf("winner = %q, want b-idle: equal pace must break toward the idle seat", best.AuthID)
	}
}

// RawPenalty is the term actually subtracted from the weighted slack, and it
// comes from the busiest window that counted.
func TestRawPenaltyTracksTheBusiestCountedWindow(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	score := ScoreAuth(cfg, seat("mixed", now,
		paced(model.WindowSession, "", now, 0.5, 0.9),
		weekly(now, 0.5, 0.2),
	), "claude-opus-5", now)

	if want := cfg.RawWeight * 0.9; math.Abs(score.RawPenalty-want) > tolerance {
		t.Fatalf("RawPenalty = %v, want %v", score.RawPenalty, want)
	}
	var weighted float64
	for _, ws := range score.Windows {
		weighted += ws.Weight * ws.Slack
	}
	if math.Abs(score.Total-(weighted-score.RawPenalty)) > tolerance {
		t.Fatalf("Total = %v, want %v", score.Total, weighted-score.RawPenalty)
	}
}

// The scoped term keys on model.FamilyOf on both sides, so provider prefixes,
// date suffixes and context markers on the model id resolve, and a Scope
// spelled as the usage endpoint's display name matches too.
func TestScopedWindowCountsOnlyForItsOwnFamily(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	cases := []struct {
		modelID string
		scope   string
		counts  bool
	}{
		{"claude-opus-5", model.FamilyOpus, true},
		{"claude-opus-5[1m]", model.FamilyOpus, true},
		{"anthropic/claude-opus-5", "Claude Opus 4", true},
		{"us.anthropic.claude-sonnet-5-v1:0", model.FamilySonnet, true},
		{"claude-3-5-haiku-latest", model.FamilyHaiku, true},
		{"Claude-Opus-5", "opus", true},
		{"claude-fable-5-1", model.FamilyOpus, false},
		{"gpt-5", model.FamilyOpus, false},
		{"claude-5", model.FamilyOpus, false},
	}
	for _, tc := range cases {
		t.Run(tc.modelID+" vs "+tc.scope, func(t *testing.T) {
			// A spent cap: it moves the score and gates only when it counts.
			snap := seat("seat", now,
				weekly(now, 0.5, 0.3),
				paced(model.WindowWeeklyScoped, tc.scope, now, 0.5, 0.99),
			)
			score := ScoreAuth(cfg, snap, tc.modelID, now)

			wantWeight := 0.0
			if tc.counts {
				wantWeight = cfg.ScopedWeight
			}
			if got := windowScoreOf(t, score, model.WindowWeeklyScoped, tc.scope).Weight; got != wantWeight {
				t.Fatalf("scoped weight = %v, want %v", got, wantWeight)
			}
			if score.Eligible == tc.counts {
				t.Fatalf("eligible=%v reason=%q, want a spent cap to gate exactly when it counts",
					score.Eligible, score.Reason)
			}
		})
	}
}

// Two live seats, read from the usage endpoint at the same instant. Seat B has
// spent its session window and most of a weekly window that resets tonight;
// seat A is barely into a weekly window that resets in a week.
func TestLiveSeatsRouteAwayFromAndThenIntoTheClosingWindow(t *testing.T) {
	cfg := model.Defaults().Pace
	const fable = "claude-fable-5-1"

	t.Run("session at the cutoff takes a seat out of the pool", func(t *testing.T) {
		now := testNow
		seatA := seat("seat-a", now,
			window(model.WindowSession, "", 0.80, at(t, "2026-09-04T22:09:59Z")),
			window(model.WindowWeekly, "", 0.04, at(t, "2026-09-11T18:59:59Z")),
			window(model.WindowWeeklyScoped, model.FamilyFable, 0, at(t, "2026-09-11T18:59:59Z")),
		)
		seatB := seat("seat-b", now,
			window(model.WindowSession, "", 1.00, at(t, "2026-09-04T22:20:00Z")),
			window(model.WindowWeekly, "", 0.54, at(t, "2026-09-05T06:00:00Z")),
			window(model.WindowWeeklyScoped, model.FamilyFable, 0.67, at(t, "2026-09-05T06:00:00Z")),
		)

		ranked := Rank(cfg, snapshots(seatA, seatB), []string{"seat-a", "seat-b"}, fable, now)
		b := scoreOf(t, ranked, "seat-b")
		if b.Eligible {
			t.Fatalf("seat-b eligible=true total=%v, want its spent session window to disqualify it", b.Total)
		}
		if b.Reason != model.ReasonHardCutoff {
			t.Fatalf("seat-b reason = %q, want %q", b.Reason, model.ReasonHardCutoff)
		}

		best, ok := Best(ranked)
		if !ok {
			t.Fatal("no eligible credential")
		}
		if best.AuthID != "seat-a" {
			t.Fatalf("winner = %q, want seat-a", best.AuthID)
		}
	})

	t.Run("weekly slack decides once the session windows reset", func(t *testing.T) {
		now := at(t, "2026-09-04T22:30:00Z")
		seatA := seat("seat-a", now,
			window(model.WindowSession, "", 0, at(t, "2026-09-05T03:09:59Z")),
			window(model.WindowWeekly, "", 0.04, at(t, "2026-09-11T18:59:59Z")),
			window(model.WindowWeeklyScoped, model.FamilyFable, 0, at(t, "2026-09-11T18:59:59Z")),
		)
		seatB := seat("seat-b", now,
			window(model.WindowSession, "", 0, at(t, "2026-09-05T03:20:00Z")),
			window(model.WindowWeekly, "", 0.54, at(t, "2026-09-05T06:00:00Z")),
			window(model.WindowWeeklyScoped, model.FamilyFable, 0.67, at(t, "2026-09-05T06:00:00Z")),
		)

		ranked := Rank(cfg, snapshots(seatA, seatB), []string{"seat-a", "seat-b"}, fable, now)
		best, ok := Best(ranked)
		if !ok {
			t.Fatal("no eligible credential")
		}
		a := scoreOf(t, ranked, "seat-a")
		b := scoreOf(t, ranked, "seat-b")
		if best.AuthID != "seat-b" {
			t.Fatalf("winner = %q (seat-a %v, seat-b %v), want seat-b: its weekly window resets in hours",
				best.AuthID, a.Total, b.Total)
		}
		// Seat B is behind a curve that is nearly over; seat A is ahead of one
		// that has just begun.
		if slack := windowScoreOf(t, b, model.WindowWeekly, "").Slack; slack <= 0 {
			t.Fatalf("seat-b weekly slack = %v, want positive", slack)
		}
		if slack := windowScoreOf(t, a, model.WindowWeekly, "").Slack; slack >= 0 {
			t.Fatalf("seat-a weekly slack = %v, want negative", slack)
		}
		// Decisive enough to pull an established binding across.
		if !ShouldSwitch(cfg, a, b) {
			t.Fatalf("ShouldSwitch(seat-a %v, seat-b %v) = false, want true", a.Total, b.Total)
		}
	})
}

// The curve exponent is a policy dial, not a constant factor: it reorders
// candidates that sit at different points in their windows.
func TestCurveExponentReordersCandidates(t *testing.T) {
	now := testNow
	early := seat("early", now, weekly(now, 0.25, 0.10))
	late := seat("late", now, weekly(now, 0.90, 0.60))
	snaps := snapshots(early, late)
	ids := []string{"early", "late"}

	cases := []struct {
		name     string
		exponent float64
		want     string
	}{
		// Front-loaded spending expects a lot early, so the lightly used seat
		// looks furthest behind.
		{"below one", 0.5, "early"},
		// Conservative early spending expects almost nothing at a quarter
		// elapsed, so the heavily used seat late in its window wins instead.
		{"above one", 3, "late"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := model.Defaults().Pace
			cfg.CurveExponent = tc.exponent
			ranked := Rank(cfg, snaps, ids, "claude-opus-5", now)
			best, ok := Best(ranked)
			if !ok {
				t.Fatal("no eligible credential")
			}
			if best.AuthID != tc.want {
				t.Fatalf("winner = %q (early %v, late %v), want %q", best.AuthID,
					scoreOf(t, ranked, "early").Total, scoreOf(t, ranked, "late").Total, tc.want)
			}
		})
	}
}

func TestRankIsDeterministicAndLeavesInputsAlone(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	snaps := snapshots(
		seat("a", now, weekly(now, 0.9, 0.1)),
		seat("b", now, weekly(now, 0.5, 0.5)),
		seat("c", now, weekly(now, 0.5, 0.5)),
		seat("d", now, weekly(now, 0.5, 1.0)),
	)
	// Eligible by total, ties broken by id, then the ineligible: "e" has no
	// snapshot and so no score, while "d" carries a deeply negative one.
	want := []string{"a", "b", "c", "e", "d"}

	orders := [][]string{
		{"a", "b", "c", "d", "e"},
		{"e", "d", "c", "b", "a"},
		{"c", "a", "e", "b", "d"},
		{"b", "e", "a", "d", "c"},
	}
	for _, order := range orders {
		candidates := append([]string{}, order...)
		before := append([]string{}, order...)
		originalWindows := append([]model.Window{}, snaps["a"].Windows...)

		got := rankedIDs(Rank(cfg, snaps, candidates, "claude-opus-5", now))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Rank(%v) = %v, want %v", order, got, want)
		}
		if !reflect.DeepEqual(candidates, before) {
			t.Fatalf("candidate ids reordered to %v, want %v", candidates, before)
		}
		if !reflect.DeepEqual(snaps["a"].Windows, originalWindows) {
			t.Fatal("snapshot windows modified")
		}
	}
}

// The host offers a candidate list, not a set. A repeated id is one credential
// and must not occupy two places in the ranking.
func TestRankScoresARepeatedCandidateOnce(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	snaps := snapshots(seat("a", now, weekly(now, 0.5, 0.2)))
	ranked := Rank(cfg, snaps, []string{"a", "ghost", "a", "ghost", "a"}, "claude-opus-5", now)

	if got, want := rankedIDs(ranked), []string{"a", "ghost"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Rank = %v, want %v", got, want)
	}
}

func TestRankScoresACandidateWithNoSnapshot(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	ranked := Rank(cfg, nil, []string{"ghost"}, "claude-opus-5", now)
	if len(ranked) != 1 {
		t.Fatalf("Rank returned %d scores, want 1", len(ranked))
	}
	if ranked[0].AuthID != "ghost" || ranked[0].Eligible || ranked[0].Reason != model.ReasonNoSnapshot {
		t.Fatalf("score = %+v, want ghost ineligible with %q", ranked[0], model.ReasonNoSnapshot)
	}
	if _, ok := Best(ranked); ok {
		t.Fatal("Best returned a credential with no snapshot")
	}
}

// Rank carries the id the host offered even when the stored snapshot disagrees,
// since that id is what the caller binds and reports.
func TestRankUsesTheCandidateID(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	snaps := map[string]model.AuthSnapshot{
		"candidate": seat("", now, weekly(now, 0.5, 0.2)),
	}
	ranked := Rank(cfg, snaps, []string{"candidate"}, "claude-opus-5", now)
	if ranked[0].AuthID != "candidate" {
		t.Fatalf("AuthID = %q, want %q", ranked[0].AuthID, "candidate")
	}
}

func TestBest(t *testing.T) {
	cases := []struct {
		name   string
		scores []model.Score
		want   string
		ok     bool
	}{
		{"empty", nil, "", false},
		{
			"none eligible",
			[]model.Score{
				{AuthID: "a", Total: 1, Reason: model.ReasonHardCutoff},
				{AuthID: "b", Total: 2, Reason: model.ReasonRejected},
			},
			"", false,
		},
		{
			"skips a higher-scoring ineligible",
			[]model.Score{
				{AuthID: "a", Total: 9},
				{AuthID: "b", Total: 1, Eligible: true},
			},
			"b", true,
		},
		{
			"unsorted input",
			[]model.Score{
				{AuthID: "a", Total: 0.1, Eligible: true},
				{AuthID: "b", Total: 0.9, Eligible: true},
				{AuthID: "c", Total: 0.5, Eligible: true},
			},
			"b", true,
		},
		{
			"ties break on id",
			[]model.Score{
				{AuthID: "z", Total: 0.5, Eligible: true},
				{AuthID: "y", Total: 0.5, Eligible: true},
			},
			"y", true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			best, ok := Best(tc.scores)
			if ok != tc.ok || best.AuthID != tc.want {
				t.Fatalf("Best = (%q, %v), want (%q, %v)", best.AuthID, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestShouldSwitch(t *testing.T) {
	eligible := func(id string, total float64) model.Score {
		return model.Score{AuthID: id, Total: total, Eligible: true}
	}

	cases := []struct {
		name       string
		margin     float64
		incumbent  model.Score
		challenger model.Score
		want       bool
	}{
		{"marginal gain holds the binding", 0.05, eligible("a", 0.50), eligible("b", 0.53), false},
		// Quarters are exactly representable, so the boundary is the boundary
		// rather than a rounding artefact.
		{"exactly the margin holds the binding", 0.25, eligible("a", 0.50), eligible("b", 0.75), false},
		{"a hair past the margin moves it", 0.25, eligible("a", 0.50), eligible("b", 0.76), true},
		{"decisive gain moves it", 0.05, eligible("a", 0.50), eligible("b", 0.70), true},
		{"a worse challenger never moves it", 0.05, eligible("a", 0.50), eligible("b", 0.10), false},
		{
			"an ineligible incumbent always yields", 0.05,
			model.Score{AuthID: "a", Total: 9, Reason: model.ReasonHardCutoff},
			eligible("b", -1),
			true,
		},
		{
			"an ineligible challenger never wins", 0.05,
			eligible("a", -5),
			model.Score{AuthID: "b", Total: 9, Reason: model.ReasonRejected},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := model.Defaults().Pace
			cfg.HysteresisMargin = tc.margin
			if got := ShouldSwitch(cfg, tc.incumbent, tc.challenger); got != tc.want {
				t.Fatalf("ShouldSwitch = %v, want %v", got, tc.want)
			}
		})
	}
}
