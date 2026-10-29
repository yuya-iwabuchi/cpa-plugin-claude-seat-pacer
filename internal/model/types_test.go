package model

import (
	"math"
	"testing"
	"time"
)

// TestScopedWindowOfAnUnknownFamilyBearsOnNoFamilylessModel covers a scoped cap
// the endpoint names for a family this build does not know: its scope maps to
// no family, as a model with no family does, and the two must not match.
func TestScopedWindowOfAnUnknownFamilyBearsOnNoFamilylessModel(t *testing.T) {
	w := Window{Kind: WindowWeeklyScoped, Scope: "Claude Nova 1"}
	if FamilyOf(w.Scope) != "" {
		t.Fatalf("FamilyOf(%q) = %q, want no known family", w.Scope, FamilyOf(w.Scope))
	}
	if w.BearsOn("") {
		t.Error("a scoped cap of an unknown family bears on a model with no family")
	}
}

func TestRiseCountsEachCycleFromItsLevelAtSince(t *testing.T) {
	midnight := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	at := func(h float64, u float64) Sample {
		return Sample{At: midnight.Add(time.Duration(h * float64(time.Hour))), Utilization: u}
	}
	// A cycle that resets at 06:00 and one that began then: 0.30 at midnight
	// rising to 0.42, then 0.05 rising to 0.20 after the reset.
	h := WindowHistory{Kind: WindowWeekly, Cycles: []Cycle{
		{ResetsAt: midnight.Add(6 * time.Hour), Samples: []Sample{at(-30, 0.1), at(-1, 0.30), at(3, 0.35), at(5.9, 0.42)}},
		{ResetsAt: midnight.Add(6*time.Hour + WeeklyDuration), Samples: []Sample{at(6.5, 0.05), at(9, math.NaN()), at(10, -math.MaxFloat64), at(12, 0.20)}},
	}}
	if got := h.Rise(midnight, WeeklyDuration); math.Abs(got-0.32) > 1e-9 {
		t.Errorf("rise since midnight = %v, want 0.12 before the reset and 0.20 after", got)
	}
	if got := h.Rise(midnight.Add(13*time.Hour), WeeklyDuration); got != 0 {
		t.Errorf("rise since after the newest sample = %v, want 0", got)
	}
	// A cycle with no sample by since counts from its first.
	late := WindowHistory{Cycles: []Cycle{{ResetsAt: midnight.Add(48 * time.Hour), Samples: []Sample{at(4, 0.5), at(8, 0.6)}}}}
	if got := late.Rise(midnight, WeeklyDuration); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("rise of a cycle first seen after since = %v, want 0.1", got)
	}
}
