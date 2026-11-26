package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/web"
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

// The page's reset menu uses a fixture seat's resets through this.
var _ web.Claimer = (*fixture)(nil)

// claimDelay is how long a fixture claim takes, about as long as the
// endpoint takes to answer one.
const claimDelay = 1200 * time.Millisecond

// ClaimReset uses one of a fixture seat's resets the way the endpoint does:
// it empties each window the grant clears, takes the family's part of the
// 7-day window off for a grant that gives it back, and answers a request id
// it has seen with its first answer.
func (f *fixture) ClaimReset(ctx context.Context, authID, grantID, requestID string) (model.ResetClaim, error) {
	select {
	case <-ctx.Done():
		return model.ResetClaim{}, ctx.Err()
	case <-time.After(claimDelay):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if res, ok := f.claims[requestID]; ok {
		return res, nil
	}
	if f.claims == nil {
		f.claims = make(map[string]model.ResetClaim)
	}
	snap, ok := f.snapshots[authID]
	if !ok || snap.Resets == nil {
		return model.ResetClaim{}, fmt.Errorf("the seat holds no resets")
	}
	grants := slices.Clone(snap.Resets.Grants)
	i := slices.IndexFunc(grants, func(g model.ResetGrant) bool { return g.ID == grantID })
	res := model.ResetClaim{Cleared: []model.WindowRef{}}
	switch {
	case i < 0:
		res.Result, res.Reason = model.ClaimUnavailable, "no_such_grant"
	case grants[i].Left < 1:
		res.Result = model.ClaimAlreadyUsed
	case !grants[i].UsableNow:
		res.Result, res.Reason = model.ClaimUnavailable, grants[i].Unavailable
	default:
		g := grants[i]
		windows := slices.Clone(snap.Windows)
		if g.GivesBack {
			fam, wk := -1, -1
			for j, w := range windows {
				switch {
				case w.Kind == model.WindowWeeklyScoped && w.Scope == g.Family:
					fam = j
				case w.Kind == model.WindowWeekly:
					wk = j
				}
			}
			if fam >= 0 && wk >= 0 {
				windows[wk].Utilization = max(0, windows[wk].Utilization-windows[fam].Utilization/2)
				windows[wk].Status, windows[wk].Severity = model.StatusAllowed, model.SeverityNormal
			}
		}
		for j, w := range windows {
			if slices.Contains(g.Clears, model.WindowRef{Kind: w.Kind, Scope: w.Scope}) {
				windows[j].Utilization = 0
				windows[j].Status, windows[j].Severity = model.StatusAllowed, model.SeverityNormal
				res.Cleared = append(res.Cleared, model.WindowRef{Kind: w.Kind, Scope: w.Scope})
			}
		}
		g.Left--
		g.UsableNow = g.Left > 0
		grants[i] = g
		left := g.Left
		res.Result, res.ResetsLeft = model.ClaimReset, &left
		snap.Windows = windows
	}
	snap.Resets = &model.ResetGrants{Grants: grants}
	f.claims[requestID] = res
	if res.Result != model.ClaimReset {
		f.snapshots[authID] = snap
		return res, nil
	}
	if f.lagging == nil {
		f.lagging = make(map[string]laggingRead)
	}
	f.lagging[authID] = laggingRead{snap: snap, from: time.Now().Add(readLag)}
	return res, nil
}

// readLag is how long after a fixture claim the seat's reading still shows
// the seat as it was, as a usage read can trail a reset the endpoint has
// answered.
const readLag = 7 * time.Second

// laggingRead is a fixture seat's reading after a claim, which a recheck
// reads from the instant from on.
type laggingRead struct {
	snap model.AuthSnapshot
	from time.Time
}

// RecheckSeat reads a fixture seat again: once a claim's lag has passed, its
// reading shows what the claim did.
func (f *fixture) RecheckSeat(_ context.Context, authID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.snapshots[authID]; !ok {
		return false, fmt.Errorf("no such seat")
	}
	if l, ok := f.lagging[authID]; ok && !time.Now().Before(l.from) {
		f.snapshots[authID] = l.snap
		delete(f.lagging, authID)
	}
	return true, nil
}
