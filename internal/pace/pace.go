// Package pace scores credentials by burn pace. For every rate-limit window it
// compares the observed utilization against the utilization a target curve
// expects at the same point in the window; the credential furthest behind its
// curve wins, which drains a window about to reset while holding one that still
// has days to run.
//
// Every function here is a pure function of its arguments: no clocks, no I/O,
// no package state. now is always a parameter.
package pace

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// Target is the utilization the curve expects at elapsed, which is clamped to
// 0..1.
func Target(cfg model.PaceConfig, elapsed float64) float64 {
	switch {
	case math.IsNaN(elapsed) || elapsed < 0:
		elapsed = 0
	case elapsed > 1:
		elapsed = 1
	}
	if cfg.CurveExponent == 1 {
		return cfg.LandingTarget * elapsed
	}
	return cfg.LandingTarget * math.Pow(elapsed, cfg.CurveExponent)
}

// ScoreAuth evaluates one credential for one model.
//
// The breakdown carries every window in the snapshot, including those that
// contribute nothing, so the status UI renders the whole picture from one
// value; a non-zero Weight marks a window that counted.
//
// A window counts when it is open — non-zero ResetsAt and positive Duration —
// and is a session or all-models weekly window, or a scoped weekly window whose
// Scope names the requested model's family. Anything else contributes no slack,
// no raw penalty and no eligibility gate: a scoped cap for another family is
// not a cap this request can reach, and a window with no open period cannot be
// placed on the curve.
func ScoreAuth(cfg model.PaceConfig, snap model.AuthSnapshot, modelID string, now time.Time) model.Score {
	score := model.Score{AuthID: snap.AuthID, Eligible: true}
	if len(snap.Windows) == 0 {
		score.Eligible = false
		score.Reason = model.ReasonNoSnapshot
		return score
	}

	family := ModelFamily(modelID)
	score.Windows = make([]model.WindowScore, 0, len(snap.Windows))

	var (
		rawUtil  float64
		counted  int
		rejected bool
		cutoff   bool
	)
	for _, w := range snap.Windows {
		elapsed := w.Elapsed(now)
		target := Target(cfg, elapsed)
		ws := model.WindowScore{
			Kind:        w.Kind,
			Scope:       w.Scope,
			Elapsed:     elapsed,
			Target:      target,
			Utilization: w.Utilization,
			Slack:       target - w.Utilization,
			ResetsAt:    w.ResetsAt,
		}
		if weight, ok := weightOf(cfg, w, family); ok {
			ws.Weight = weight
			score.Total += weight * ws.Slack
			counted++
			if w.Utilization > rawUtil {
				rawUtil = w.Utilization
			}
			rejected = rejected || w.Blocking()
			cutoff = cutoff || w.Utilization >= cfg.HardCutoff
		}
		score.Windows = append(score.Windows, ws)
	}

	// The raw term rides alongside pace because slack alone under-penalizes a
	// credential at 80% that is merely on schedule, which starves idle
	// siblings.
	score.RawPenalty = cfg.RawWeight * rawUtil
	score.Total -= score.RawPenalty

	switch {
	case counted == 0:
		// Windows exist but none bear on this request, so there is no pace
		// signal to route on.
		score.Eligible = false
		score.Reason = model.ReasonNoSnapshot
	case rejected:
		score.Eligible = false
		score.Reason = model.ReasonRejected
	case cutoff:
		score.Eligible = false
		score.Reason = model.ReasonHardCutoff
	}
	return score
}

// weightOf reports the coefficient a window contributes with, and whether it
// counts at all.
func weightOf(cfg model.PaceConfig, w model.Window, family string) (float64, bool) {
	if w.ResetsAt.IsZero() || w.Duration <= 0 {
		return 0, false
	}
	switch w.Kind {
	case model.WindowSession:
		return cfg.SessionWeight, true
	case model.WindowWeekly:
		return cfg.WeeklyWeight, true
	case model.WindowWeeklyScoped:
		if family != "" && strings.EqualFold(w.Scope, family) {
			return cfg.ScopedWeight, true
		}
	}
	return 0, false
}

// Rank scores every candidate and orders the results: eligible before
// ineligible, then Total descending, then AuthID ascending so repeated calls on
// the same inputs agree regardless of the order the host offered candidates in.
// A candidate with no snapshot scores ineligible with model.ReasonNoSnapshot.
// The arguments are not modified.
func Rank(cfg model.PaceConfig, snaps map[string]model.AuthSnapshot, candidateIDs []string, modelID string, now time.Time) []model.Score {
	scores := make([]model.Score, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		snap, ok := snaps[id]
		if !ok {
			scores = append(scores, model.Score{AuthID: id, Reason: model.ReasonNoSnapshot})
			continue
		}
		s := ScoreAuth(cfg, snap, modelID, now)
		// The candidate id the host offered is authoritative; a snapshot may
		// carry an empty or differently spelled AuthID.
		s.AuthID = id
		scores = append(scores, s)
	}
	sort.Slice(scores, func(i, j int) bool { return better(scores[i], scores[j]) })
	return scores
}

// better is the ranking order: eligible first, then higher Total, then lower
// AuthID.
func better(a, b model.Score) bool {
	if a.Eligible != b.Eligible {
		return a.Eligible
	}
	if a.Total != b.Total {
		return a.Total > b.Total
	}
	return a.AuthID < b.AuthID
}

// Best is the highest-ranked eligible score, and false when none is eligible.
// The slice need not be sorted.
func Best(scores []model.Score) (model.Score, bool) {
	var best model.Score
	found := false
	for _, s := range scores {
		if !s.Eligible {
			continue
		}
		if !found || better(s, best) {
			best, found = s, true
		}
	}
	return best, found
}

// ShouldSwitch reports whether an established binding moves to challenger. An
// ineligible incumbent always yields; otherwise the challenger must win by more
// than HysteresisMargin, without which routing flaps every time a window
// boundary nudges two credentials past each other. An ineligible challenger
// never wins.
func ShouldSwitch(cfg model.PaceConfig, incumbent model.Score, challenger model.Score) bool {
	if !challenger.Eligible {
		return false
	}
	if !incumbent.Eligible {
		return true
	}
	return challenger.Total > incumbent.Total+cfg.HysteresisMargin
}

// Stale reports whether a snapshot is too old to route on. PaceConfig carries no
// staleness bound, so scoring never gates on age: the caller supplies maxAge
// from QuotaConfig.MaxStaleness and applies model.ReasonStale itself.
func Stale(snap model.AuthSnapshot, now time.Time, maxAge time.Duration) bool {
	return snap.Stale(now, maxAge)
}
