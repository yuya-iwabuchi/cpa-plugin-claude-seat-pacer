package pace

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

const tolerance = 1e-9

func at(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse %q: %v", ts, err)
	}
	return parsed
}

// window places a window on the clock so that Elapsed(now) is the requested
// fraction, which lets a test state the pace position it cares about instead of
// a reset instant.
func window(kind model.WindowKind, scope string, now time.Time, elapsed, util float64) model.Window {
	duration := model.WeeklyDuration
	if kind == model.WindowSession {
		duration = model.SessionDuration
	}
	return model.Window{
		Kind:        kind,
		Scope:       scope,
		Utilization: util,
		Duration:    duration,
		ResetsAt:    now.Add(time.Duration(float64(duration) * (1 - elapsed))),
	}
}

func weekly(now time.Time, elapsed, util float64) model.Window {
	return window(model.WindowWeekly, "", now, elapsed, util)
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
	now := at(t, "2026-09-04T19:45:00Z")

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
	now := at(t, "2026-09-04T19:45:00Z")

	snap := seat("seat", now,
		window(model.WindowSession, "", now, 0.5, 0.2),
		weekly(now, 0.25, 0.1),
		window(model.WindowWeeklyScoped, FamilyOpus, now, 0.25, 0.6),
	)
	score := ScoreAuth(cfg, snap, "claude-fable-5-1", now)

	if len(score.Windows) != len(snap.Windows) {
		t.Fatalf("breakdown has %d windows, want %d", len(score.Windows), len(snap.Windows))
	}
	for i, ws := range score.Windows {
		src := snap.Windows[i]
		if ws.Kind != src.Kind || ws.Scope != src.Scope {
			t.Fatalf("window %d = %s/%q, want %s/%q", i, ws.Kind, ws.Scope, src.Kind, src.Scope)
		}
		if !ws.ResetsAt.Equal(src.ResetsAt) {
			t.Fatalf("window %d ResetsAt = %v, want %v", i, ws.ResetsAt, src.ResetsAt)
		}
		if ws.Utilization != src.Utilization {
			t.Fatalf("window %d Utilization = %v, want %v", i, ws.Utilization, src.Utilization)
		}
		if math.Abs(ws.Elapsed-src.Elapsed(now)) > tolerance {
			t.Fatalf("window %d Elapsed = %v, want %v", i, ws.Elapsed, src.Elapsed(now))
		}
		if math.Abs(ws.Target-Target(cfg, ws.Elapsed)) > tolerance {
			t.Fatalf("window %d Target = %v, want %v", i, ws.Target, Target(cfg, ws.Elapsed))
		}
		if math.Abs(ws.Slack-(ws.Target-ws.Utilization)) > tolerance {
			t.Fatalf("window %d Slack = %v, want %v", i, ws.Slack, ws.Target-ws.Utilization)
		}
	}

	if got := windowScoreOf(t, score, model.WindowSession, "").Weight; got != cfg.SessionWeight {
		t.Fatalf("session weight = %v, want %v", got, cfg.SessionWeight)
	}
	if got := windowScoreOf(t, score, model.WindowWeekly, "").Weight; got != cfg.WeeklyWeight {
		t.Fatalf("weekly weight = %v, want %v", got, cfg.WeeklyWeight)
	}
	// An Opus cap is invisible to a Fable request, so it is rendered but
	// carries no weight.
	if got := windowScoreOf(t, score, model.WindowWeeklyScoped, FamilyOpus).Weight; got != 0 {
		t.Fatalf("non-matching scoped weight = %v, want 0", got)
	}
}

func TestScoreAuthEligibility(t *testing.T) {
	cfg := model.Defaults().Pace
	now := at(t, "2026-09-04T19:45:00Z")

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
		{"no windows", nil, false, model.ReasonNoSnapshot},
		{
			"only another family's cap",
			[]model.Window{window(model.WindowWeeklyScoped, FamilyOpus, now, 0.5, 0.2)},
			false,
			model.ReasonNoSnapshot,
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
}

// A window with no open period cannot be placed on the curve, so it neither
// scores nor gates, even when its recorded utilization would otherwise be
// disqualifying.
func TestZeroResetsAtWindowContributesNothing(t *testing.T) {
	cfg := model.Defaults().Pace
	now := at(t, "2026-09-04T19:45:00Z")

	live := weekly(now, 0.5, 0.2)
	dead := model.Window{Kind: model.WindowSession, Utilization: 1.0, Duration: model.SessionDuration}

	withoutDead := ScoreAuth(cfg, seat("seat", now, live), "claude-opus-5", now)
	withDead := ScoreAuth(cfg, seat("seat", now, live, dead), "claude-opus-5", now)

	if withDead.Total != withoutDead.Total {
		t.Fatalf("total with an unopened window = %v, want %v", withDead.Total, withoutDead.Total)
	}
	if withDead.RawPenalty != withoutDead.RawPenalty {
		t.Fatalf("raw penalty = %v, want %v", withDead.RawPenalty, withoutDead.RawPenalty)
	}
	if !withDead.Eligible {
		t.Fatalf("eligible=false reason=%q, want an unopened window not to gate", withDead.Reason)
	}
	if got := windowScoreOf(t, withDead, model.WindowSession, "").Weight; got != 0 {
		t.Fatalf("unopened window weight = %v, want 0", got)
	}
}

// Two credentials can both sit exactly on their curves while one is nearly
// spent and the other is idle. Pace slack alone calls that a tie; the raw term
// is what sends the request to the idle seat.
func TestRawPenaltySpreadsLoadBetweenSeatsOnPace(t *testing.T) {
	cfg := model.Defaults().Pace
	now := at(t, "2026-09-04T19:45:00Z")

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

	// RawPenalty is the term actually subtracted, taken from the busiest window
	// that counted.
	score := ScoreAuth(cfg, seat("mixed", now,
		window(model.WindowSession, "", now, 0.5, 0.9),
		weekly(now, 0.5, 0.2),
	), "claude-opus-5", now)
	if want := cfg.RawWeight * 0.9; math.Abs(score.RawPenalty-want) > tolerance {
		t.Fatalf("RawPenalty = %v, want %v", score.RawPenalty, want)
	}
	var paced float64
	for _, ws := range score.Windows {
		paced += ws.Weight * ws.Slack
	}
	if math.Abs(score.Total-(paced-score.RawPenalty)) > tolerance {
		t.Fatalf("Total = %v, want %v", score.Total, paced-score.RawPenalty)
	}
}

func TestScopedWindowForAnotherFamilyIsIgnored(t *testing.T) {
	cfg := model.Defaults().Pace
	now := at(t, "2026-09-04T19:45:00Z")

	base := []model.Window{
		window(model.WindowSession, "", now, 0.5, 0.2),
		weekly(now, 0.5, 0.3),
	}
	withOpusCap := append(append([]model.Window{}, base...),
		window(model.WindowWeeklyScoped, FamilyOpus, now, 0.5, 0.99))

	plain := ScoreAuth(cfg, seat("seat", now, base...), "claude-fable-5-1", now)
	scoped := ScoreAuth(cfg, seat("seat", now, withOpusCap...), "claude-fable-5-1", now)

	if !scoped.Eligible {
		t.Fatalf("eligible=false reason=%q, want a spent Opus cap not to block a Fable request",
			scoped.Reason)
	}
	if scoped.Total != plain.Total {
		t.Fatalf("total = %v, want %v: another family's cap must not move the score",
			scoped.Total, plain.Total)
	}

	// The same cap decides the score once the request is for its family.
	forOpus := ScoreAuth(cfg, seat("seat", now, withOpusCap...), "claude-opus-5", now)
	if forOpus.Eligible {
		t.Fatal("eligible=true, want an Opus request blocked by a spent Opus cap")
	}
	if forOpus.Reason != model.ReasonHardCutoff {
		t.Fatalf("reason = %q, want %q", forOpus.Reason, model.ReasonHardCutoff)
	}
}

// Two live seats, read from the usage endpoint at the same instant. Seat B has
// spent its session window and most of a weekly window that resets tonight;
// seat A is barely into a weekly window that resets in a week.
func TestLiveSeatsRouteAwayFromAndThenIntoTheClosingWindow(t *testing.T) {
	cfg := model.Defaults().Pace
	const fable = "claude-fable-5-1"

	t.Run("session at the cutoff takes a seat out of the pool", func(t *testing.T) {
		now := at(t, "2026-09-04T19:45:00Z")
		seatA := seat("seat-a", now,
			model.Window{
				Kind: model.WindowSession, Utilization: 0.80,
				ResetsAt: at(t, "2026-09-04T22:09:59Z"), Duration: model.SessionDuration,
			},
			model.Window{
				Kind: model.WindowWeekly, Utilization: 0.04,
				ResetsAt: at(t, "2026-09-11T18:59:59Z"), Duration: model.WeeklyDuration,
			},
			model.Window{
				Kind: model.WindowWeeklyScoped, Scope: FamilyFable, Utilization: 0,
				ResetsAt: at(t, "2026-09-11T18:59:59Z"), Duration: model.WeeklyDuration,
			},
		)
		seatB := seat("seat-b", now,
			model.Window{
				Kind: model.WindowSession, Utilization: 1.00,
				ResetsAt: at(t, "2026-09-04T22:20:00Z"), Duration: model.SessionDuration,
			},
			model.Window{
				Kind: model.WindowWeekly, Utilization: 0.54,
				ResetsAt: at(t, "2026-09-05T06:00:00Z"), Duration: model.WeeklyDuration,
			},
			model.Window{
				Kind: model.WindowWeeklyScoped, Scope: FamilyFable, Utilization: 0.67,
				ResetsAt: at(t, "2026-09-05T06:00:00Z"), Duration: model.WeeklyDuration,
			},
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
			model.Window{
				Kind: model.WindowSession, Utilization: 0,
				ResetsAt: at(t, "2026-09-05T03:09:59Z"), Duration: model.SessionDuration,
			},
			model.Window{
				Kind: model.WindowWeekly, Utilization: 0.04,
				ResetsAt: at(t, "2026-09-11T18:59:59Z"), Duration: model.WeeklyDuration,
			},
			model.Window{
				Kind: model.WindowWeeklyScoped, Scope: FamilyFable, Utilization: 0,
				ResetsAt: at(t, "2026-09-11T18:59:59Z"), Duration: model.WeeklyDuration,
			},
		)
		seatB := seat("seat-b", now,
			model.Window{
				Kind: model.WindowSession, Utilization: 0,
				ResetsAt: at(t, "2026-09-05T03:20:00Z"), Duration: model.SessionDuration,
			},
			model.Window{
				Kind: model.WindowWeekly, Utilization: 0.54,
				ResetsAt: at(t, "2026-09-05T06:00:00Z"), Duration: model.WeeklyDuration,
			},
			model.Window{
				Kind: model.WindowWeeklyScoped, Scope: FamilyFable, Utilization: 0.67,
				ResetsAt: at(t, "2026-09-05T06:00:00Z"), Duration: model.WeeklyDuration,
			},
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
	now := at(t, "2026-09-04T19:45:00Z")
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
	now := at(t, "2026-09-04T19:45:00Z")

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

func TestRankScoresACandidateWithNoSnapshot(t *testing.T) {
	cfg := model.Defaults().Pace
	now := at(t, "2026-09-04T19:45:00Z")

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
	now := at(t, "2026-09-04T19:45:00Z")

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
	cfg := model.Defaults().Pace
	eligible := func(id string, total float64) model.Score {
		return model.Score{AuthID: id, Total: total, Eligible: true}
	}

	cases := []struct {
		name       string
		incumbent  model.Score
		challenger model.Score
		want       bool
	}{
		{"marginal gain holds the binding", eligible("a", 0.50), eligible("b", 0.53), false},
		{"exactly the margin holds the binding", eligible("a", 0.50), eligible("b", 0.55), false},
		{"decisive gain moves it", eligible("a", 0.50), eligible("b", 0.70), true},
		{"a worse challenger never moves it", eligible("a", 0.50), eligible("b", 0.10), false},
		{
			"an ineligible incumbent always yields",
			model.Score{AuthID: "a", Total: 9, Reason: model.ReasonHardCutoff},
			eligible("b", -1),
			true,
		},
		{
			"an ineligible challenger never wins",
			eligible("a", -5),
			model.Score{AuthID: "b", Total: 9, Reason: model.ReasonRejected},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldSwitch(cfg, tc.incumbent, tc.challenger); got != tc.want {
				t.Fatalf("ShouldSwitch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStale(t *testing.T) {
	now := at(t, "2026-09-04T19:45:00Z")
	fresh := model.AuthSnapshot{ObservedAt: now.Add(-time.Minute)}
	old := model.AuthSnapshot{ObservedAt: now.Add(-time.Hour)}

	if Stale(fresh, now, 15*time.Minute) {
		t.Fatal("a one-minute-old snapshot reported stale")
	}
	if !Stale(old, now, 15*time.Minute) {
		t.Fatal("an hour-old snapshot reported fresh")
	}
	if !Stale(model.AuthSnapshot{}, now, 15*time.Minute) {
		t.Fatal("a snapshot that was never observed reported fresh")
	}

	// Age is the caller's policy: scoring itself ignores it.
	cfg := model.Defaults().Pace
	score := ScoreAuth(cfg, seat("seat", now.Add(-24*time.Hour), weekly(now, 0.5, 0.2)), "claude-opus-5", now)
	if !score.Eligible {
		t.Fatalf("eligible=false reason=%q, want scoring to ignore snapshot age", score.Reason)
	}
}
