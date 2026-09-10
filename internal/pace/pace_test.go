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

// unopened is a bearing window with a length but no reset instant, which is
// how a reading arrives before the provider has named a period for it. It has
// no place on the curve, so it carries no weight and no term in the cost.
func unopened(kind model.WindowKind, scope string, util float64) model.Window {
	return model.Window{Kind: kind, Scope: scope, Utilization: util, Duration: duration(kind)}
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
	// A curve landing exactly at full is the reference the others bend away
	// from, and the one the tilt is measured against.
	flat := model.Defaults().Pace
	flat.LandingTarget = 1

	tilted := model.Defaults().Pace

	steep := flat
	steep.Shape = model.ShapePower
	steep.CurveExponent = 2

	shallow := flat
	shallow.Shape = model.ShapePower
	shallow.CurveExponent = 0.5

	cautious := flat
	cautious.LandingTarget = 0.9

	sigmoid := flat
	sigmoid.Shape = model.ShapeSigmoid

	cases := []struct {
		name    string
		cfg     model.PaceConfig
		elapsed float64
		want    float64
	}{
		// A linear curve landing at 1.0 makes target and elapsed the same
		// number.
		{"linear start", flat, 0, 0},
		{"linear half", flat, 0.5, 0.5},
		{"linear nine tenths", flat, 0.9, 0.9},
		{"linear end", flat, 1, 1},
		{"clamps below zero", flat, -0.5, 0},
		{"clamps above one", flat, 1.5, 1},
		{"exponent above one lands late", steep, 0.5, 0.25},
		{"exponent below one lands early", shallow, 0.25, 0.5},
		{"landing target scales the curve", cautious, 0.5, 0.45},
		{"landing target caps the end", cautious, 1, 0.9},
		// The default landing runs the curve a tenth ahead of elapsed, until
		// the clamp catches it a landing-reciprocal of the way through.
		{"the tilt starts at zero all the same", tilted, 0, 0},
		{"the tilt runs ahead of elapsed", tilted, 0.5, 0.55},
		{"the tilt saturates at 1/landing", tilted, 1 / 1.10, 1},
		{"the clamp holds it there", tilted, 0.95, 1},
		{"the clamp holds it at the close", tilted, 1, 1},
		// The sigmoid is exact at both ends and crosses linear at the middle.
		{"sigmoid start", sigmoid, 0, 0},
		{"sigmoid middle", sigmoid, 0.5, 0.5},
		{"sigmoid end", sigmoid, 1, 1},
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

// The clamp is what stops the perishable-budget tilt rewarding a credential
// with nothing left, so it has to hold for every curve, every landing
// Normalize admits, and every elapsed a caller can reach it with.
func TestTargetNeverExceedsFull(t *testing.T) {
	for _, shape := range []string{model.ShapeLinear, model.ShapePower, model.ShapeSigmoid, "unrecognised"} {
		for _, landing := range []float64{0.5, 1, 1.10, 2, 4} {
			for _, exponent := range []float64{0.25, 1, 3} {
				cfg := model.Defaults().Pace
				cfg.Shape = shape
				cfg.LandingTarget = landing
				cfg.CurveExponent = exponent
				for step := -2; step <= 12; step++ {
					elapsed := float64(step) / 10
					got := Target(cfg, elapsed)
					if math.IsNaN(got) || got < 0 || got > 1 {
						t.Fatalf("Target(shape=%s landing=%v exponent=%v, elapsed=%v) = %v, want 0..1",
							shape, landing, exponent, elapsed, got)
					}
				}
			}
		}
	}
}

// Each shape is the unit curve a target rides, so they are pinned against each
// other rather than against absolute numbers.
func TestShapes(t *testing.T) {
	linear := model.Defaults().Pace

	power := linear
	power.Shape = model.ShapePower
	power.CurveExponent = 1

	sigmoid := linear
	sigmoid.Shape = model.ShapeSigmoid

	steps := []float64{0, 0.1, 0.25, 0.4, 0.5, 0.6, 0.75, 0.9, 1}

	t.Run("linear is the identity", func(t *testing.T) {
		for _, e := range steps {
			if got := shape(linear, e); math.Abs(got-e) > tolerance {
				t.Errorf("shape(linear, %v) = %v, want %v", e, got, e)
			}
		}
	})

	t.Run("power at exponent one is linear", func(t *testing.T) {
		for _, e := range steps {
			if got, want := shape(power, e), shape(linear, e); math.Abs(got-want) > tolerance {
				t.Errorf("shape(power^1, %v) = %v, want the linear %v", e, got, want)
			}
		}
	})

	// Both ends are exact rather than near, so a window at its open has no
	// target to be behind and one at its close targets the whole landing.
	t.Run("sigmoid is exact at both ends", func(t *testing.T) {
		if got := shape(sigmoid, 0); got != 0 {
			t.Errorf("shape(sigmoid, 0) = %v, want exactly 0", got)
		}
		if got := shape(sigmoid, 1); got != 1 {
			t.Errorf("shape(sigmoid, 1) = %v, want exactly 1", got)
		}
	})

	t.Run("sigmoid holds back early and makes it up late", func(t *testing.T) {
		for _, e := range []float64{0.1, 0.25, 0.4} {
			if got := shape(sigmoid, e); got >= e {
				t.Errorf("shape(sigmoid, %v) = %v, want below the linear %v", e, got, e)
			}
		}
		if got := shape(sigmoid, 0.5); math.Abs(got-0.5) > tolerance {
			t.Errorf("shape(sigmoid, 0.5) = %v, want it to cross linear at the midpoint", got)
		}
		for _, e := range []float64{0.6, 0.75, 0.9} {
			if got := shape(sigmoid, e); got <= e {
				t.Errorf("shape(sigmoid, %v) = %v, want above the linear %v", e, got, e)
			}
		}
	})

	t.Run("steepness deepens the S", func(t *testing.T) {
		shallow, steep := sigmoid, sigmoid
		shallow.Steepness = 2
		steep.Steepness = 16
		if got, want := shape(steep, 0.25), shape(shallow, 0.25); got >= want {
			t.Errorf("shape(k=16, 0.25) = %v, want further below the shallower %v", got, want)
		}
	})
}

// Weekly quota is perishable: it expires at reset rather than carrying over.
// Two credentials exactly on a flat curve — nine tenths through a week with
// nine tenths spent, and a tenth through with a tenth spent — are not equally
// good picks, because only one of them holds budget that survives the night.
// The landing above full is what breaks that tie toward the closing window.
func TestPerishableBudgetTiltsThePickTowardTheClosingWindow(t *testing.T) {
	now := testNow

	// Named so that an alphabetical tie-break would pick the fresh seat.
	closing := seat("b-closing", now, weekly(now, 0.9, 0.9))
	fresh := seat("a-fresh", now, weekly(now, 0.1, 0.1))
	snaps := snapshots(closing, fresh)
	ids := []string{"a-fresh", "b-closing"}

	// A curve landing exactly at full calls it a tie: both seats sit on the
	// line, so both cost nothing and the id decides.
	flat := model.Defaults().Pace
	flat.LandingTarget = 1
	flatRanked := Rank(flat, snaps, ids, "claude-opus-5", now)
	for _, id := range ids {
		if cost := scoreOf(t, flatRanked, id).Cost; math.Abs(cost) > tolerance {
			t.Fatalf("%s cost on a flat curve = %v, want a seat exactly on pace", id, cost)
		}
	}

	cfg := model.Defaults().Pace
	if cfg.LandingTarget <= 1 {
		t.Fatalf("LandingTarget = %v, want the default landing above full", cfg.LandingTarget)
	}
	ranked := Rank(cfg, snaps, ids, "claude-opus-5", now)
	closingCost := scoreOf(t, ranked, "b-closing").Cost
	freshCost := scoreOf(t, ranked, "a-fresh").Cost
	if closingCost >= freshCost {
		t.Fatalf("closing cost %v, fresh cost %v, want the closing window to cost less",
			closingCost, freshCost)
	}
	best, ok := Best(ranked)
	if !ok || best.AuthID != "b-closing" {
		t.Fatalf("winner = (%q, %v), want b-closing: its budget expires first", best.AuthID, ok)
	}
}

// The clamp bounds the tilt and fixes where it stops climbing: the target
// reaches full at the landing's reciprocal and holds there, so a window at its
// cap has a slack of exactly zero rather than a negative one. Bounding the tilt
// is all it does. A cost of zero is cheaper than any credential running over
// its own curve, so the clamp does not order a credential with nothing left
// behind one with budget; the fullness gate is what takes the two full seats
// below out of the pool.
func TestClampBoundsTheTiltAndFixesWhereItSaturates(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	saturates := 1 / cfg.LandingTarget
	if target := Target(cfg, saturates); target != 1 {
		t.Fatalf("Target(%v) = %v, want exactly full at the landing's reciprocal", saturates, target)
	}
	if target := Target(cfg, saturates*0.99); target >= 1 {
		t.Fatalf("Target(%v) = %v, want the curve still climbing below the reciprocal",
			saturates*0.99, target)
	}

	// Past the saturation point, where a credential with nothing left would
	// win on cost alone.
	const late = 0.95
	if target := Target(cfg, late); target != 1 {
		t.Fatalf("Target(%v) = %v, want the tilt saturated at full", late, target)
	}

	// Named so that an alphabetical tie-break would pick the spent seat.
	spent := seat("a-spent", now, weekly(now, late, 1.0))
	over := seat("b-over", now, weekly(now, late, 1.2))
	headroom := seat("c-headroom", now, weekly(now, late, 0.999))

	ids := []string{"a-spent", "b-over", "c-headroom"}
	ranked := Rank(cfg, snapshots(spent, over, headroom), ids, "claude-opus-5", now)

	spentScore := scoreOf(t, ranked, "a-spent")
	if slack := windowScoreOf(t, spentScore, model.WindowWeekly, "").Slack; slack != 0 {
		t.Fatalf("spent slack = %v, want exactly 0: the clamp holds its target at full", slack)
	}
	if spentScore.Cost != 0 {
		t.Fatalf("spent cost = %v, want exactly 0", spentScore.Cost)
	}
	if got := scoreOf(t, ranked, "b-over").Cost; got <= spentScore.Cost {
		t.Fatalf("over-cap cost = %v, want above the spent seat's %v", got, spentScore.Cost)
	}

	// Neither seat has anything left to spend, so neither is in the pool, and
	// the reason names the reading rather than a forecast.
	for _, id := range []string{"a-spent", "b-over"} {
		s := scoreOf(t, ranked, id)
		if s.Eligible || s.Reason != model.ReasonSpent {
			t.Fatalf("%s: eligible=%v reason=%q, want eligible=false reason=%q",
				id, s.Eligible, s.Reason, model.ReasonSpent)
		}
	}
	if s := scoreOf(t, ranked, "c-headroom"); !s.Eligible {
		t.Fatalf("c-headroom: eligible=false reason=%q, want a thousandth of headroom to be budget",
			s.Reason)
	}

	best, ok := Best(ranked)
	if !ok || best.AuthID != "c-headroom" {
		t.Fatalf("winner = (%q, %v), want c-headroom: the only seat with anything left",
			best.AuthID, ok)
	}
	if got, want := rankedIDs(ranked), []string{"c-headroom", "a-spent", "b-over"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Rank = %v, want %v", got, want)
	}
}

// The trap a clamped slack of zero sets, and the reason a full window has to
// gate. Each window is measured at its own elapsed fraction, because each
// seat's week resets at its own time, so cost does not order credentials by the
// budget they hold: a seat with nothing left targets full, so its slack is zero
// and it costs nothing, which beats any seat running over its own curve however
// much budget that seat still holds. Running over your own curve is the
// ordinary state just after serving a burst. Both seats sit at different points
// in their weeks here, which is the whole of the trap: pin them to one elapsed
// fraction and cost orders them correctly by accident.
func TestASeatWithNothingLeftLosesToOneOverItsOwnCurve(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	// Named so that an alphabetical tie-break would pick the spent seat. The
	// second seat holds seven tenths of its budget a fifth of the way into its
	// week, which puts it over its own curve.
	spent := seat("a-spent", now, weekly(now, 0.958, 1.0))
	budgeted := seat("b-70pct-left", now, weekly(now, 0.202, 0.30))
	ids := []string{"a-spent", "b-70pct-left"}
	ranked := Rank(cfg, snapshots(spent, budgeted), ids, "claude-opus-5", now)

	spentScore := scoreOf(t, ranked, "a-spent")
	budgetedScore := scoreOf(t, ranked, "b-70pct-left")

	// The trap, stated as an assertion: on cost alone the seat with nothing
	// left is the cheaper of the two.
	if spentScore.Cost != 0 {
		t.Fatalf("spent cost = %v, want the clamped zero this turns on", spentScore.Cost)
	}
	if budgetedScore.Cost <= spentScore.Cost {
		t.Fatalf("budgeted cost = %v, want above the spent seat's %v: a seat over its own curve costs more than a clamped zero",
			budgetedScore.Cost, spentScore.Cost)
	}

	// So the gate, not the cost, is what orders them.
	if spentScore.Eligible || spentScore.Reason != model.ReasonSpent {
		t.Fatalf("spent: eligible=%v reason=%q, want eligible=false reason=%q",
			spentScore.Eligible, spentScore.Reason, model.ReasonSpent)
	}
	if !budgetedScore.Eligible {
		t.Fatalf("budgeted: eligible=false reason=%q, want a seat holding most of its budget in the pool",
			budgetedScore.Reason)
	}
	best, ok := Best(ranked)
	if !ok || best.AuthID != "b-70pct-left" {
		t.Fatalf("winner = (%q, %v), want b-70pct-left: the only seat with anything to spend",
			best.AuthID, ok)
	}
	// A binding already on the spent seat moves too, rather than holding it
	// until the provider refuses.
	if !ShouldSwitch(cfg, spentScore, budgetedScore) {
		t.Fatal("ShouldSwitch(spent incumbent, budgeted challenger) = false, want true")
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
		t.Fatalf("winner = %q (ranked %v), want the closing window with headroom",
			best.AuthID, rankedIDs(ranked))
	}
}

func TestScoreAuthBreakdownCoversEveryWindow(t *testing.T) {
	cfg := model.Defaults().Pace
	cfg.SessionWeight = 0.35
	now := testNow

	snap := seat("seat", now,
		paced(model.WindowSession, "", now, 0.5, 0.2),
		weekly(now, 0.25, 0.1),
		paced(model.WindowWeeklyScoped, model.FamilyOpus, now, 0.25, 0.6),
	)
	score := ScoreAuth(cfg, snap, "claude-fable-5-1", now)

	// The default curve is linear landing at 1.10, so target is elapsed and a
	// tenth again, and every number below is arithmetic a reader can redo by
	// hand.
	want := []model.WindowScore{
		{Kind: model.WindowSession, Elapsed: 0.5, Target: 0.55, Utilization: 0.2, Slack: 0.35, Weight: cfg.SessionWeight},
		{Kind: model.WindowWeekly, Elapsed: 0.25, Target: 0.275, Utilization: 0.1, Slack: 0.175, Weight: cfg.WeeklyWeight},
		// An Opus cap is invisible to a Fable request, so it is rendered but
		// carries no weight.
		{Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus, Elapsed: 0.25, Target: 0.275, Utilization: 0.6, Slack: -0.325},
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

	// -(0.35*0.35 + 1.0*0.175): both counted windows sit under their target,
	// so the credential is cheap to route to and nothing else enters the sum.
	if wantCost := -0.2975; math.Abs(score.Cost-wantCost) > tolerance {
		t.Fatalf("Cost = %v, want %v", score.Cost, wantCost)
	}
}

func TestScoreAuthEligibility(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	rejected := weekly(now, 0.5, 0.5)
	rejected.Status = model.StatusRejected

	critical := weekly(now, 0.5, 0.5)
	critical.Severity = model.SeverityCritical

	rejectedAndFull := weekly(now, 0.5, 1.0)
	rejectedAndFull.Status = model.StatusRejected

	criticalAndFull := weekly(now, 0.5, 1.0)
	criticalAndFull.Severity = model.SeverityCritical

	warned := weekly(now, 0.5, 0.5)
	warned.Status = model.StatusAllowedWarning
	warned.Severity = model.SeverityWarning

	cases := []struct {
		name     string
		windows  []model.Window
		eligible bool
		reason   string
		// dear marks a credential whose windows run over their target, so its
		// cost is positive: any credential under its own curve outranks it.
		dear bool
	}{
		{"healthy", []model.Window{weekly(now, 0.5, 0.2)}, true, model.ReasonEligible, false},
		{"warning is not blocking", []model.Window{warned}, true, model.ReasonEligible, false},
		{"provider rejected", []model.Window{rejected}, false, model.ReasonRejected, false},
		{"severity critical is not a gate", []model.Window{critical}, true, model.ReasonEligible, false},
		// A full window is the provider's own report that this cap has nothing
		// left, so it gates. Being over target is not that report: a window a
		// thousandth short of full is dear to route to and still routable.
		{"a hair under full is dear but eligible", []model.Window{weekly(now, 0.5, 0.999)}, true, model.ReasonEligible, true},
		{"full gates", []model.Window{weekly(now, 0.5, 1.0)}, false, model.ReasonSpent, true},
		{"past full gates too", []model.Window{weekly(now, 0.5, 1.5)}, false, model.ReasonSpent, true},
		// Reason order: a recorded refusal is the more specific fact about a
		// window that is both full and refused.
		{"a rejection outranks fullness", []model.Window{rejectedAndFull}, false, model.ReasonRejected, true},
		{"fullness gates whatever the severity says", []model.Window{criticalAndFull}, false, model.ReasonSpent, true},
		{"no windows", nil, false, model.ReasonNoWindow, false},
		{
			"only another family's cap",
			[]model.Window{paced(model.WindowWeeklyScoped, model.FamilyOpus, now, 0.5, 0.2)},
			false,
			model.ReasonNoWindow,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := ScoreAuth(cfg, seat("seat", now, tc.windows...), "claude-fable-5-1", now)
			if score.Eligible != tc.eligible || score.Reason != tc.reason {
				t.Fatalf("eligible=%v reason=%q, want eligible=%v reason=%q",
					score.Eligible, score.Reason, tc.eligible, tc.reason)
			}
			if math.IsNaN(score.Cost) || math.IsInf(score.Cost, 0) {
				t.Fatalf("cost = %v, want a finite score", score.Cost)
			}
			if got := score.Cost > 0; got != tc.dear {
				t.Fatalf("cost = %v, want a positive cost = %v", score.Cost, tc.dear)
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

// The gate is the provider's own report that a window is full, so its threshold
// is exactly full and nothing configures it. A window a thousandth short of
// full is a window with something left in it.
func TestFullnessGatesAtExactlyFullAndNoSooner(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	cases := []struct {
		name     string
		util     float64
		eligible bool
	}{
		{"nine tenths", 0.9, true},
		{"three nines", 0.999, true},
		{"the last step below full", math.Nextafter(1, 0), true},
		{"exactly full", 1, false},
		{"the first step past full", math.Nextafter(1, 2), false},
		{"half again", 1.5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantReason := model.ReasonEligible
			if !tc.eligible {
				wantReason = model.ReasonSpent
			}
			score := ScoreAuth(cfg, seat("seat", now, weekly(now, 0.5, tc.util)), "claude-opus-5", now)
			if score.Eligible != tc.eligible || score.Reason != wantReason {
				t.Fatalf("at %v: eligible=%v reason=%q, want eligible=%v reason=%q",
					tc.util, score.Eligible, score.Reason, tc.eligible, wantReason)
			}
		})
	}
}

// The gate reads Utilization and nothing else. The usage endpoint never writes
// Status, and quota.Store.Put replaces a credential's windows wholesale, so the
// poll that follows a refusal drops the rejected status along with the window
// that carried it. A gate keyed on Status would hand a credential with nothing
// left back to the pool on that poll.
func TestFullnessGatesWithNoStatusSoAUsagePollCannotClearIt(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow
	const opus = "claude-opus-5"

	refused := weekly(now, 0.5, 1.0)
	refused.Status = model.StatusRejected
	if score := ScoreAuth(cfg, seat("seat", now, refused), opus, now); score.Eligible {
		t.Fatal("eligible=true on a refused window, want it gated before the poll lands")
	}

	// What the usage endpoint writes in its place: the same window, full, with
	// neither a status nor a severity to condemn it.
	polled := weekly(now, 0.5, 1.0)
	if polled.Status != "" || polled.Severity != "" {
		t.Fatalf("fixture = status %q severity %q, want both unset as the usage endpoint leaves them",
			polled.Status, polled.Severity)
	}
	if polled.Blocking() || polled.Critical() {
		t.Fatal("fixture reads as blocking or critical, want a window only its utilization condemns")
	}

	score := ScoreAuth(cfg, seat("seat", now, polled), opus, now)
	if score.Eligible || score.Reason != model.ReasonSpent {
		t.Fatalf("eligible=%v reason=%q, want eligible=false reason=%q",
			score.Eligible, score.Reason, model.ReasonSpent)
	}
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

	idle := func(util float64) model.Window { return unopened(model.WindowSession, "", util) }

	// Whatever such a window reads, it has no place on the curve, so it moves
	// neither the weight nor the cost.
	t.Run("contributes no slack or weight at any utilization", func(t *testing.T) {
		withoutIdle := ScoreAuth(cfg, seat("seat", now, live), "claude-opus-5", now)
		for _, util := range []float64{0, 0.9, 1.0, 1.5} {
			withIdle := ScoreAuth(cfg, seat("seat", now, live, idle(util)), "claude-opus-5", now)
			if withIdle.Cost != withoutIdle.Cost {
				t.Errorf("cost with an unopened window at %v = %v, want %v",
					util, withIdle.Cost, withoutIdle.Cost)
			}
			if got := windowScoreOf(t, withIdle, model.WindowSession, "").Weight; got != 0 {
				t.Errorf("at %v: unopened window weight = %v, want 0", util, got)
			}
		}
	})

	t.Run("a reading under full does not gate", func(t *testing.T) {
		for _, util := range []float64{0, 0.9, 0.999} {
			score := ScoreAuth(cfg, seat("seat", now, live, idle(util)), "claude-opus-5", now)
			if !score.Eligible {
				t.Errorf("at %v: eligible=false reason=%q, want an unopened window with room not to gate",
					util, score.Reason)
			}
		}
	})

	t.Run("provider rejection gates", func(t *testing.T) {
		dead := idle(0)
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

	t.Run("another family's unopened cap does not gate", func(t *testing.T) {
		opusCap := unopened(model.WindowWeeklyScoped, model.FamilyOpus, 1.0)
		if score := ScoreAuth(cfg, seat("seat", now, live, opusCap), "claude-fable-5-1", now); !score.Eligible {
			t.Fatalf("eligible=false reason=%q, want an Opus cap not to gate a Fable request", score.Reason)
		}
	})

	// A seat whose only reading is a rejection is out of the pool, not the
	// winner of a one-candidate field.
	t.Run("a rejected seat never wins", func(t *testing.T) {
		dead := idle(0)
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

// The fullness gate sits outside the open-period guard, so a bearing window
// with no period on the clock is read for fullness all the same. Such a window
// has no place on the curve — no weight, and no term in the cost — and a
// provider reporting it full has still reported that this cap has nothing left.
func TestFullnessIsReadOnAWindowWithNoOpenPeriod(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow
	const opus = "claude-opus-5"
	live := weekly(now, 0.5, 0.1)
	bare := ScoreAuth(cfg, seat("seat", now, live), opus, now)

	// A scoped cap, so the window it stands in for is one that would carry a
	// weight if it had a period.
	if cfg.ScopedWeight == 0 {
		t.Fatalf("ScopedWeight = %v, want a non-zero weight for this to be a window that could carry one",
			cfg.ScopedWeight)
	}

	for _, util := range []float64{1.0, 1.5} {
		full := unopened(model.WindowWeeklyScoped, model.FamilyOpus, util)
		if !full.ResetsAt.IsZero() || full.Duration <= 0 {
			t.Fatalf("fixture = ResetsAt %v Duration %v, want a length and no reset instant",
				full.ResetsAt, full.Duration)
		}

		score := ScoreAuth(cfg, seat("seat", now, live, full), opus, now)
		if score.Eligible || score.Reason != model.ReasonSpent {
			t.Fatalf("at %v: eligible=%v reason=%q, want eligible=false reason=%q",
				util, score.Eligible, score.Reason, model.ReasonSpent)
		}
		if got := windowScoreOf(t, score, model.WindowWeeklyScoped, model.FamilyOpus).Weight; got != 0 {
			t.Fatalf("at %v: weight = %v, want 0: it has no place on the curve", util, got)
		}
		if score.Cost != bare.Cost {
			t.Fatalf("at %v: cost = %v, want the live window's own %v", util, score.Cost, bare.Cost)
		}
	}
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
func TestRankOrdersUnreadableCostsLast(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	snaps := snapshots(
		seat("good-high", now, weekly(now, 0.9, 0.1)),
		seat("good-low", now, weekly(now, 0.5, 0.4)),
		// Sorts before the dear seat by id, after it by eligibility order.
		seat("nan-a", now, weekly(now, 0.5, math.NaN())),
		seat("nan-b", now, weekly(now, 0.5, math.NaN())),
		// A hair short of full, so it is still in the pool and dear enough to
		// sort behind both good seats.
		seat("y-dear", now, weekly(now, 0.5, 0.999)),
	)
	want := []string{"good-high", "good-low", "y-dear", "nan-a", "nan-b"}

	orders := [][]string{
		{"good-high", "good-low", "y-dear", "nan-a", "nan-b"},
		{"nan-a", "nan-b", "y-dear", "good-low", "good-high"},
		{"nan-b", "good-low", "y-dear", "nan-a", "good-high"},
		{"y-dear", "nan-a", "good-high", "nan-b", "good-low"},
	}
	for _, order := range orders {
		ranked := Rank(cfg, snaps, append([]string{}, order...), "claude-opus-5", now)
		if got := rankedIDs(ranked); !reflect.DeepEqual(got, want) {
			t.Fatalf("Rank(%v) = %v, want %v", order, got, want)
		}
	}

	ranked := Rank(cfg, snaps, orders[0], "claude-opus-5", now)
	// The order above is an eligible dear seat ahead of two unreadable ones,
	// not one ineligible seat ahead of another.
	if dear := scoreOf(t, ranked, "y-dear"); !dear.Eligible || dear.Cost <= 0 {
		t.Fatalf("y-dear = eligible %v cost %v, want an eligible seat with a positive cost",
			dear.Eligible, dear.Cost)
	}
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

// Cost is the weighted window sum and nothing else: no penalty term, no
// tie-break, no bonus for a credential that happens to be idle. It is
// reconstructible from the breakdown the same score carries.
func TestCostIsTheWeightedWindowSumAndNothingElse(t *testing.T) {
	now := testNow

	cfg := model.Defaults().Pace
	cfg.SessionWeight = 0.35

	// A bearing session window, the all-models weekly, this request's scoped
	// cap, another family's cap, and a bearing window with no period. Only the
	// first three carry a weight, and the terms run in both directions.
	snap := seat("mixed", now,
		paced(model.WindowSession, "", now, 0.5, 0.9),
		weekly(now, 0.95, 0.4),
		paced(model.WindowWeeklyScoped, model.FamilyOpus, now, 0.95, 1.2),
		paced(model.WindowWeeklyScoped, model.FamilySonnet, now, 0.3, 0.7),
		model.Window{Kind: model.WindowWeekly, Utilization: 0.5, Duration: model.WeeklyDuration},
	)
	score := ScoreAuth(cfg, snap, "claude-opus-5", now)

	wantWeights := []float64{cfg.SessionWeight, cfg.WeeklyWeight, cfg.ScopedWeight, 0, 0}
	if len(score.Windows) != len(wantWeights) {
		t.Fatalf("breakdown has %d windows, want %d", len(score.Windows), len(wantWeights))
	}
	var want float64
	for i, ws := range score.Windows {
		if ws.Weight != wantWeights[i] {
			t.Fatalf("window %d weight = %v, want %v", i, ws.Weight, wantWeights[i])
		}
		want -= ws.Weight * ws.Slack
	}
	if math.Abs(score.Cost-want) > tolerance {
		t.Fatalf("Cost = %v, want -sum(weight*slack) = %v", score.Cost, want)
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
			// A full cap: it moves the cost, and gates, only when it counts.
			snap := seat("seat", now,
				weekly(now, 0.5, 0.3),
				paced(model.WindowWeeklyScoped, tc.scope, now, 0.5, 1.0),
			)
			score := ScoreAuth(cfg, snap, tc.modelID, now)
			bare := ScoreAuth(cfg, seat("seat", now, weekly(now, 0.5, 0.3)), tc.modelID, now)

			wantWeight := 0.0
			if tc.counts {
				wantWeight = cfg.ScopedWeight
			}
			scoped := windowScoreOf(t, score, model.WindowWeeklyScoped, tc.scope)
			if scoped.Weight != wantWeight {
				t.Fatalf("scoped weight = %v, want %v", scoped.Weight, wantWeight)
			}
			if want := -wantWeight * scoped.Slack; math.Abs(score.Cost-bare.Cost-want) > tolerance {
				t.Fatalf("the scoped cap moved the cost by %v, want %v", score.Cost-bare.Cost, want)
			}
			wantReason := model.ReasonSpent
			if !tc.counts {
				wantReason = model.ReasonEligible
			}
			if score.Eligible == tc.counts || score.Reason != wantReason {
				t.Fatalf("eligible=%v reason=%q, want a full cap to gate exactly when it counts",
					score.Eligible, score.Reason)
			}
		})
	}
}

// Two live seats, read from the usage endpoint at the same instant. Seat B has
// nearly spent its session window and most of a weekly window that resets
// tonight; seat A is barely into a weekly window that resets in a week. Seat
// B's remaining weekly budget is the perishable one, so both readings route to
// it.
func TestLiveSeatsRouteIntoTheClosingWindow(t *testing.T) {
	cfg := model.Defaults().Pace
	const fable = "claude-fable-5-1"

	// The two readings, parameterised on the session window because that is
	// the only reading the first two cases differ in.
	readSeatA := func(t *testing.T, now time.Time, session float64) model.AuthSnapshot {
		return seat("seat-a", now,
			window(model.WindowSession, "", session, at(t, "2026-09-04T22:09:59Z")),
			window(model.WindowWeekly, "", 0.04, at(t, "2026-09-11T18:59:59Z")),
			window(model.WindowWeeklyScoped, model.FamilyFable, 0, at(t, "2026-09-11T18:59:59Z")),
		)
	}
	readSeatB := func(t *testing.T, now time.Time, session float64) model.AuthSnapshot {
		return seat("seat-b", now,
			window(model.WindowSession, "", session, at(t, "2026-09-04T22:20:00Z")),
			window(model.WindowWeekly, "", 0.54, at(t, "2026-09-05T06:00:00Z")),
			window(model.WindowWeeklyScoped, model.FamilyFable, 0.67, at(t, "2026-09-05T06:00:00Z")),
		)
	}

	t.Run("a nearly spent session window does not take a seat out of the pool", func(t *testing.T) {
		now := testNow
		seatA := readSeatA(t, now, 0.80)
		seatB := readSeatB(t, now, 0.99)

		ranked := Rank(cfg, snapshots(seatA, seatB), []string{"seat-a", "seat-b"}, fable, now)
		b := scoreOf(t, ranked, "seat-b")
		if !b.Eligible {
			t.Fatalf("seat-b eligible=false reason=%q, want a session window with room not to gate",
				b.Reason)
		}
		// The session window is a rate limit rather than a budget, so it
		// carries no weight and the pick still turns on the weekly windows.
		if got := windowScoreOf(t, b, model.WindowSession, "").Weight; got != 0 {
			t.Fatalf("seat-b session weight = %v, want 0", got)
		}

		best, ok := Best(ranked)
		if !ok {
			t.Fatal("no eligible credential")
		}
		if best.AuthID != "seat-b" {
			t.Fatalf("winner = %q (seat-a %v, seat-b %v), want seat-b: its weekly headroom expires tonight",
				best.AuthID, scoreOf(t, ranked, "seat-a").Cost, b.Cost)
		}
	})

	// The same two seats, with seat B's session window at its cap. The
	// provider says that seat can serve nothing for the next few hours, so its
	// perishable weekly budget no longer buys it the request.
	t.Run("a spent session window does take a seat out of the pool", func(t *testing.T) {
		now := testNow
		seatA := readSeatA(t, now, 0.80)
		seatB := readSeatB(t, now, 1.00)

		ranked := Rank(cfg, snapshots(seatA, seatB), []string{"seat-a", "seat-b"}, fable, now)
		b := scoreOf(t, ranked, "seat-b")
		if b.Eligible || b.Reason != model.ReasonSpent {
			t.Fatalf("seat-b eligible=%v reason=%q, want eligible=false reason=%q",
				b.Eligible, b.Reason, model.ReasonSpent)
		}
		// Gated on the reading, not on a weight: the window still contributes
		// nothing to the cost, and seat B is still the cheaper of the two.
		if got := windowScoreOf(t, b, model.WindowSession, "").Weight; got != 0 {
			t.Fatalf("seat-b session weight = %v, want 0", got)
		}
		a := scoreOf(t, ranked, "seat-a")
		if b.Cost >= a.Cost {
			t.Fatalf("seat-a cost %v, seat-b cost %v, want seat-b still the cheaper: only the gate keeps it out",
				a.Cost, b.Cost)
		}

		best, ok := Best(ranked)
		if !ok || best.AuthID != "seat-a" {
			t.Fatalf("winner = (%q, %v), want seat-a: seat-b can serve nothing until its session resets",
				best.AuthID, ok)
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
				best.AuthID, a.Cost, b.Cost)
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
			t.Fatalf("ShouldSwitch(seat-a %v, seat-b %v) = false, want true", a.Cost, b.Cost)
		}
	})
}

// The curve exponent is a policy dial, not a constant factor: on the power
// shape it reorders candidates that sit at different points in their windows.
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
			cfg.Shape = model.ShapePower
			cfg.CurveExponent = tc.exponent
			ranked := Rank(cfg, snaps, ids, "claude-opus-5", now)
			best, ok := Best(ranked)
			if !ok {
				t.Fatal("no eligible credential")
			}
			if best.AuthID != tc.want {
				t.Fatalf("winner = %q (early %v, late %v), want %q", best.AuthID,
					scoreOf(t, ranked, "early").Cost, scoreOf(t, ranked, "late").Cost, tc.want)
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
		seat("d", now, weekly(now, 0.5, 0.99)),
		seat("f", now, weekly(now, 0.5, 1.0)),
	)
	// Eligible by cost, ties broken by id, then the ineligible by the same
	// rule: "d" runs over its target and so is the dearest seat still in the
	// pool, "e" has no snapshot and scores zero, and "f" is spent and carries
	// the cost that puts it last.
	want := []string{"a", "b", "c", "d", "e", "f"}

	orders := [][]string{
		{"a", "b", "c", "d", "e", "f"},
		{"f", "e", "d", "c", "b", "a"},
		{"c", "a", "e", "b", "f", "d"},
		{"b", "e", "a", "d", "c", "f"},
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
				{AuthID: "a", Cost: 1, Reason: model.ReasonBadReading},
				{AuthID: "b", Cost: 2, Reason: model.ReasonRejected},
			},
			"", false,
		},
		{
			"skips a cheaper ineligible",
			[]model.Score{
				{AuthID: "a", Cost: 0.1},
				{AuthID: "b", Cost: 1, Eligible: true},
			},
			"b", true,
		},
		{
			"unsorted input",
			[]model.Score{
				{AuthID: "a", Cost: 0.1, Eligible: true},
				{AuthID: "b", Cost: 0.9, Eligible: true},
				{AuthID: "c", Cost: 0.5, Eligible: true},
			},
			"a", true,
		},
		{
			"ties break on id",
			[]model.Score{
				{AuthID: "z", Cost: 0.5, Eligible: true},
				{AuthID: "y", Cost: 0.5, Eligible: true},
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
	eligible := func(id string, cost float64) model.Score {
		return model.Score{AuthID: id, Cost: cost, Eligible: true}
	}

	cases := []struct {
		name       string
		margin     float64
		incumbent  model.Score
		challenger model.Score
		want       bool
	}{
		{"marginal saving holds the binding", 0.05, eligible("a", 0.50), eligible("b", 0.47), false},
		// Quarters are exactly representable, so the boundary is the boundary
		// rather than a rounding artefact.
		{"exactly the margin holds the binding", 0.25, eligible("a", 0.50), eligible("b", 0.25), false},
		{"a hair past the margin moves it", 0.25, eligible("a", 0.50), eligible("b", 0.24), true},
		{"decisive saving moves it", 0.05, eligible("a", 0.50), eligible("b", 0.30), true},
		{"a dearer challenger never moves it", 0.05, eligible("a", 0.50), eligible("b", 0.90), false},
		{
			"an ineligible incumbent always yields", 0.05,
			model.Score{AuthID: "a", Cost: -1, Reason: model.ReasonStale},
			eligible("b", 9),
			true,
		},
		{
			"an ineligible challenger never wins", 0.05,
			eligible("a", 9),
			model.Score{AuthID: "b", Cost: -5, Reason: model.ReasonRejected},
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

// The split the default weights rely on: the session window is a rate limit
// rather than a budget, so no reading of it moves the cost, whatever the
// reading says. Eligibility is the other half of the split, and it turns only
// on what the provider has reported — a refusal recorded against the window,
// or a window it reports as full.
func TestSessionWindowPacesNothingAndGatesOnWhatTheProviderReports(t *testing.T) {
	cfg := model.Defaults().Pace
	now := testNow

	if cfg.SessionWeight != 0 {
		t.Fatalf("SessionWeight = %v, want 0", cfg.SessionWeight)
	}

	bare := ScoreAuth(cfg, seat("bare", now, weekly(now, 0.5, 0.1)), "claude-opus-5", now)
	busy := func(util float64) model.Score {
		return ScoreAuth(cfg, seat("busy", now,
			paced(model.WindowSession, "", now, 0.5, util),
			weekly(now, 0.5, 0.1),
		), "claude-opus-5", now)
	}

	// Costing nothing holds at every reading, including a full one: the gate
	// below is not a weight in disguise.
	for _, util := range []float64{0, 0.2, 1.0, 1.5} {
		if score := busy(util); score.Cost != bare.Cost {
			t.Errorf("cost with a session window at %v = %v, want the weekly window's %v",
				util, score.Cost, bare.Cost)
		}
	}

	for _, util := range []float64{0, 0.2, 0.9, 0.999} {
		if score := busy(util); !score.Eligible {
			t.Errorf("session window at %v: eligible=false reason=%q, want a window with room not to gate",
				util, score.Reason)
		}
	}

	for _, util := range []float64{1.0, 1.5} {
		score := busy(util)
		if score.Eligible || score.Reason != model.ReasonSpent {
			t.Errorf("session window at %v: eligible=%v reason=%q, want eligible=false reason=%q",
				util, score.Eligible, score.Reason, model.ReasonSpent)
		}
	}

	refused := paced(model.WindowSession, "", now, 0.5, 0.4)
	refused.Status = model.StatusRejected
	score := ScoreAuth(cfg, seat("refused", now, refused, weekly(now, 0.5, 0.1)), "claude-opus-5", now)
	if score.Eligible || score.Reason != model.ReasonRejected {
		t.Fatalf("eligible=%v reason=%q, want a refused session window to gate",
			score.Eligible, score.Reason)
	}
}

// A credential whose usage read failed is not a credential without caps: the
// first tells an operator to look at the endpoint, the second at the model.
func TestFailedReadIsNotAMissingCap(t *testing.T) {
	now := time.Now().UTC()
	cfg := model.Defaults().Pace

	read := model.AuthSnapshot{AuthID: "seat", ObservedAt: now}
	if score := ScoreAuth(cfg, read, "claude-fable-5-1", now); score.Reason != model.ReasonNoWindow {
		t.Errorf("reason = %q, want %q", score.Reason, model.ReasonNoWindow)
	}

	failed := model.AuthSnapshot{AuthID: "seat", ObservedAt: now, Err: "usage endpoint throttled"}
	score := ScoreAuth(cfg, failed, "claude-fable-5-1", now)
	if score.Eligible || score.Reason != model.ReasonFetchFailed {
		t.Errorf("eligible=%v reason=%q, want eligible=false reason=%q",
			score.Eligible, score.Reason, model.ReasonFetchFailed)
	}
}

// Cost counts against a credential, so a credential nothing is known about
// scores zero, which is the cheapest a real one can be. Eligibility is what
// keeps it out of the running, and this pins that: an unread credential must
// never take a request from one carrying an honest cost.
func TestAnUnreadCredentialNeverLooksCheapest(t *testing.T) {
	now := time.Now().UTC()
	cfg := model.Defaults().Pace

	read := seat("read", now, weekly(now, 0.5, 0.9))
	unread := model.AuthSnapshot{AuthID: "unread", ObservedAt: now, Err: "usage endpoint throttled"}

	ranked := Rank(cfg, snapshots(read, unread), []string{"read", "unread"}, "claude-opus-5", now)
	if got := scoreOf(t, ranked, "unread"); got.Cost != 0 || got.Eligible {
		t.Fatalf("unread = cost %v eligible %v, want zero cost and ineligible", got.Cost, got.Eligible)
	}
	if got := scoreOf(t, ranked, "read"); got.Cost <= 0 {
		t.Fatalf("read = cost %v, want a positive cost so the zero is the cheaper number", got.Cost)
	}

	best, ok := Best(ranked)
	if !ok || best.AuthID != "read" {
		t.Fatalf("Best = %q (%v), want the read credential despite its dearer cost", best.AuthID, ok)
	}
	if ranked[0].AuthID != "read" {
		t.Errorf("Rank put %q first, want the read credential", ranked[0].AuthID)
	}
}

// A window carrying no weight contributes no term, so an unreadable reading on
// one must not reach Cost. It still gates the credential — the reading is
// unusable — but the cost it reports has to stay a number the status page can
// print, because the page derives its own total from the weighted windows and
// would otherwise disagree with the scorer.
func TestAnUnreadableZeroWeightWindowLeavesCostFinite(t *testing.T) {
	now := time.Now().UTC()
	cfg := model.Defaults().Pace
	if cfg.SessionWeight != 0 {
		t.Fatalf("session weight = %v, want 0 so this test exercises a zero-weight window", cfg.SessionWeight)
	}

	snap := seat("seat", now,
		paced(model.WindowSession, "", now, 0.5, math.NaN()),
		weekly(now, 0.5, 0.4),
	)
	score := ScoreAuth(cfg, snap, "claude-opus-5", now)

	if math.IsNaN(score.Cost) || math.IsInf(score.Cost, 0) {
		t.Errorf("cost = %v, want a finite number", score.Cost)
	}
	// The weekly window alone sets it, and the reading still gates.
	weeklyOnly := ScoreAuth(cfg, seat("seat", now, weekly(now, 0.5, 0.4)), "claude-opus-5", now)
	if math.Abs(score.Cost-weeklyOnly.Cost) > tolerance {
		t.Errorf("cost = %v, want the weighted windows' own total %v", score.Cost, weeklyOnly.Cost)
	}
	if score.Eligible || score.Reason != model.ReasonBadReading {
		t.Errorf("eligible=%v reason=%q, want ineligible with %q",
			score.Eligible, score.Reason, model.ReasonBadReading)
	}
}
