// Package pace scores credentials against the burn-pace curve that
// model.PaceConfig defines and documents.
//
// Every function here is a pure function of its arguments: no clocks, no I/O,
// no package state. Where time matters it arrives as a now argument.
package pace

import (
	"math"
	"sort"
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
	return cfg.LandingTarget * math.Pow(elapsed, cfg.CurveExponent)
}

// ScoreAuth evaluates one credential for one model.
//
// A window bears on the request when it is a session or an all-models weekly
// window, or a scoped weekly window whose Scope names the requested model's
// family; a scoped cap for another family is not a cap this request can reach.
// Every bearing window gates, open or not: a provider rejection, a utilization
// at or above HardCutoff, or a utilization that is not a finite number makes
// the credential ineligible. Only a bearing window with an open period —
// non-zero ResetsAt and positive Duration — has a place on the curve, so only
// it carries a weight, a raw-utilization reading and a contribution to Total.
//
// The breakdown carries every window in the snapshot, including those that
// contribute nothing, so the status UI renders the whole picture from one
// value; a non-zero Weight marks a window that moved the score.
//
// Snapshot age is not gated here; the caller applies QuotaConfig.MaxStaleness
// and model.ReasonStale.
func ScoreAuth(cfg model.PaceConfig, snap model.AuthSnapshot, modelID string, now time.Time) model.Score {
	score := model.Score{AuthID: snap.AuthID, Eligible: true}
	if len(snap.Windows) == 0 {
		score.Eligible = false
		// A snapshot with no window and an error has never been read, which is
		// a different fact from a credential whose caps do not cover the model.
		score.Reason = model.ReasonNoWindow
		if snap.Err != "" {
			score.Reason = model.ReasonFetchFailed
		}
		return score
	}

	family := model.FamilyOf(modelID)
	score.Windows = make([]model.WindowScore, 0, len(snap.Windows))

	var (
		rawUtil    float64
		bearing    int
		rejected   bool
		cutoff     bool
		unreadable bool
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
		if w.BearsOn(family) {
			bearing++
			rejected = rejected || w.Blocking()
			cutoff = cutoff || w.Utilization >= cfg.HardCutoff
			unreadable = unreadable || math.IsNaN(w.Utilization) || math.IsInf(w.Utilization, 0)
			if open(w) {
				ws.Weight = weightOf(cfg, w)
				score.Cost -= ws.Weight * ws.Slack
				if w.Utilization > rawUtil {
					rawUtil = w.Utilization
				}
			}
		}
		score.Windows = append(score.Windows, ws)
	}

	score.FullestPenalty = cfg.RawWeight * rawUtil
	score.Cost += score.FullestPenalty

	switch {
	case bearing == 0:
		score.Eligible = false
		score.Reason = model.ReasonNoWindow
	case rejected:
		score.Eligible = false
		score.Reason = model.ReasonRejected
	case unreadable:
		score.Eligible = false
		score.Reason = model.ReasonBadReading
	case cutoff:
		score.Eligible = false
		score.Reason = model.ReasonHardCutoff
	}
	return score
}

// open reports whether a window has a period to place on the curve.
func open(w model.Window) bool {
	return !w.ResetsAt.IsZero() && w.Duration > 0
}

// weightOf is the coefficient an open, bearing window contributes with.
func weightOf(cfg model.PaceConfig, w model.Window) float64 {
	switch w.Kind {
	case model.WindowSession:
		return cfg.SessionWeight
	case model.WindowWeekly:
		return cfg.WeeklyWeight
	case model.WindowWeeklyScoped:
		return cfg.ScopedWeight
	}
	return 0
}

// Rank scores every candidate and orders the results: eligible before
// ineligible, then Total descending, then AuthID ascending so repeated calls on
// the same inputs agree regardless of the order the host offered candidates in.
// A repeated id is scored and returned once. A candidate with no snapshot
// scores ineligible with model.ReasonNoSnapshot. The arguments are not
// modified.
func Rank(cfg model.PaceConfig, snaps map[string]model.AuthSnapshot, candidateIDs []string, modelID string, now time.Time) []model.Score {
	scores := make([]model.Score, 0, len(candidateIDs))
	seen := make(map[string]struct{}, len(candidateIDs))
	for _, id := range candidateIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
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

// better is the ranking order: eligible first, then a comparable Cost ahead of
// a NaN one, then lower Cost, then lower AuthID. NaN compares false against
// every number, so it needs its own branch for the order to stay transitive.
func better(a, b model.Score) bool {
	if a.Eligible != b.Eligible {
		return a.Eligible
	}
	aNaN, bNaN := math.IsNaN(a.Cost), math.IsNaN(b.Cost)
	if aNaN != bNaN {
		return bNaN
	}
	if !aNaN && a.Cost != b.Cost {
		return a.Cost < b.Cost
	}
	return a.AuthID < b.AuthID
}

// Best is the cheapest eligible credential, and false when none is eligible.
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
// than HysteresisMargin. An ineligible challenger never wins.
func ShouldSwitch(cfg model.PaceConfig, incumbent model.Score, challenger model.Score) bool {
	if !challenger.Eligible {
		return false
	}
	if !incumbent.Eligible {
		return true
	}
	return challenger.Cost < incumbent.Cost-cfg.HysteresisMargin
}
