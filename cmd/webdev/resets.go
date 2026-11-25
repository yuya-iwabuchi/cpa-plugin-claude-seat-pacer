package main

import (
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// fableResetLife is how long after the fixture's instant its Fable resets
// expire: a launch grant lasts months.
const fableResetLife = 105 * 24 * time.Hour

// grantSpec describes the reset grants one fixture seat holds: full resets
// left and spent, expiring fullIn after the fixture's instant, and Fable
// resets left.
type grantSpec struct {
	full, fullSpent, fable int
	fullIn                 time.Duration
}

// resets builds the grants block a usage read reports for spec, or nil for
// the zero spec, which stands for a read that reported none.
func (spec grantSpec) resets(anchor time.Time) *model.ResetGrants {
	if spec == (grantSpec{}) {
		return nil
	}
	session := model.WindowRef{Kind: model.WindowSession}
	weekly := model.WindowRef{Kind: model.WindowWeekly}
	fable := model.WindowRef{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable}
	out := &model.ResetGrants{Grants: []model.ResetGrant{}}
	if spec.full+spec.fullSpent > 0 {
		ends := anchor.Add(spec.fullIn)
		out.Grants = append(out.Grants, model.ResetGrant{
			ID: "full-reset", Type: model.ResetTypeFull, Left: spec.full, EndsAt: &ends,
			Clears: []model.WindowRef{session, weekly, fable}, UsableNow: spec.full > 0,
		})
	}
	ends := anchor.Add(fableResetLife)
	for i := 0; i < spec.fable; i++ {
		id := "launch-fable"
		if i > 0 {
			id = "launch-fable-tenure"
		}
		out.Grants = append(out.Grants, model.ResetGrant{
			ID: id, Type: model.ResetTypeFull, Family: model.FamilyFable, Left: 1, EndsAt: &ends, Clears: []model.WindowRef{session, fable}, UsableNow: true, GivesBack: true,
		})
	}
	return out
}

// resetsScenario builds a pool of seats holding limit resets, named for the
// state of the windows a reset would clear.
func (f *fixture) resetsScenario() {
	f.auths, f.bindings = nil, nil
	f.snapshots = map[string]model.AuthSnapshot{}
	f.snapshotsPast = map[string]model.AuthSnapshot{}
	f.observedAge = map[string]time.Duration{}
	day := 24.0
	seats := []manySeat{
		{label: "7d full, 3d left", session: 0.30, weekly: 1.0, scoped: 0.62, elapsedDays: 4, state: "weekly-rejected",
			grants: grantSpec{full: 1, fable: 1, fullIn: 16 * 24 * time.Hour}},
		{label: "7d 88%", session: 0.22, weekly: 0.88, scoped: 0.30, elapsedDays: 5.3,
			grants: grantSpec{full: 1, fable: 1, fullIn: 16 * 24 * time.Hour}},
		{label: "Fable refused, 5h 41%", session: 0.41, weekly: 0.71, scoped: 1.0, elapsedDays: 2.5, state: "scoped-rejected",
			grants: grantSpec{fullSpent: 1, fable: 1, fullIn: 16 * 24 * time.Hour}},
		{label: "7d 52%", session: 0.10, weekly: 0.52, scoped: 0.20, elapsedDays: 3.5,
			grants: grantSpec{full: 1, fable: 1, fullIn: 2*24*time.Hour + 5*time.Hour}},
		{label: "7d full, resets in 6h", session: 0.05, weekly: 1.0, scoped: 0.70, elapsedDays: 7 - 6/day, state: "weekly-rejected",
			grants: grantSpec{full: 1, fable: 1, fullIn: 16 * 24 * time.Hour}},
		{label: "7d full, 3.8d left", session: 0.15, weekly: 1.0, scoped: 0.45, elapsedDays: 3.2, state: "weekly-rejected",
			grants: grantSpec{fullSpent: 1, fable: 1, fullIn: 16 * 24 * time.Hour}},
		{label: "Fable refused, 7d 64%", session: 0.27, weekly: 0.64, scoped: 1.0, elapsedDays: 2.8, state: "scoped-rejected",
			grants: grantSpec{fullSpent: 1, fable: 2, fullIn: 16 * 24 * time.Hour}},
		{label: "7d 40%", session: 0.18, weekly: 0.40, scoped: 0.25, elapsedDays: 1.5,
			grants: grantSpec{fullSpent: 1, fable: 2, fullIn: 16 * 24 * time.Hour}},
		{label: "7d 33%", session: 0.12, weekly: 0.33, scoped: 0.10, elapsedDays: 2,
			grants: grantSpec{fullSpent: 1, fullIn: 16 * 24 * time.Hour}},
		{label: "7d 21%", session: 0.08, weekly: 0.21, scoped: 0.05, elapsedDays: 0.8},
	}
	ids := make([]string, 0, len(seats))
	for i, ms := range seats {
		ms.id = "claude-resets-" + string(rune('a'+i)) + ".json"
		ms.name = ms.id
		f.addManySeat(i, ms)
		ids = append(ids, ms.id)
	}
	f.decisions = f.manyDecisions(ids)
}
