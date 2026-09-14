package pace

import (
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

const tolerance = 1e-9

// The two model ids the tests route on: one in the Opus family, one in Fable,
// so a scoped weekly cap on either names the other's requests as out of scope.
const (
	opus  = "claude-opus-5"
	fable = "claude-fable-5-1"
)

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
