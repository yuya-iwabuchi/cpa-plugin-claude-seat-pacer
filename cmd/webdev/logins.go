package main

import (
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// loginSeat is one seat of the login scenarios: its many-scenario shape and
// the login state the scenario gives it.
type loginSeat struct {
	seat manySeat
	team string
	// state is a model.Login* state.
	state string
	// access is when the seat's access token runs out, from the anchor.
	access time.Duration
	// loggedIn is how long before the anchor the plugin saw the login made;
	// zero for a login made before the plugin was watching.
	loggedIn time.Duration
}

func ptr(t time.Time) *time.Time { return &t }

// loginPool builds a pool from seats and applies each one's login state: a
// failing seat still serves, and an expired one is listed unavailable with no
// end, keeps the reading it last took and holds no conversation.
func (f *fixture) loginPool(seats []loginSeat) {
	f.auths, f.bindings = nil, nil
	f.snapshots = map[string]model.AuthSnapshot{}
	f.snapshotsPast = map[string]model.AuthSnapshot{}
	f.observedAge = map[string]time.Duration{}
	ids := make([]string, 0, len(seats))
	for i, ls := range seats {
		f.addManySeat(i, ls.seat)
		ids = append(ids, ls.seat.id)
	}
	f.decisions = f.manyDecisions(ids)
	for i, ls := range seats {
		a := &f.auths[i]
		login := &model.Login{State: ls.state, Team: ls.team, AccessExpiresAt: ptr(f.anchor.Add(ls.access))}
		if ls.loggedIn > 0 {
			login.LoggedInAt = ptr(f.anchor.Add(-ls.loggedIn))
			login.EndsAround = ptr(f.anchor.Add(-ls.loggedIn + model.LoginLifetime))
		}
		a.Login = login
		if ls.state != model.LoginExpired {
			continue
		}
		a.HostStatus = "unavailable"
		a.Bindings = 0
		snap := f.snapshots[a.AuthID]
		if -ls.access > time.Hour {
			snap.Err, snap.ErrCategory = "quota: rate-limited (http 429): usage endpoint throttled", "rate-limited"
		} else {
			snap.Err, snap.ErrCategory = "quota: auth (http 401): credential rejected", "auth"
		}
		f.snapshots[a.AuthID] = snap
		f.observedAge[a.AuthID] = -ls.access + 2*time.Minute
		// Each conversation the seat held moved on its next request.
		moved := 0
		for j := range f.bindings {
			b := &f.bindings[j]
			if b.AuthID != a.AuthID {
				continue
			}
			to := ids[(i+1+moved)%len(ids)]
			for _, other := range seats {
				if other.seat.id == to && other.state == model.LoginExpired {
					to = ""
				}
			}
			at := f.anchor.Add(ls.access + time.Duration(4+9*moved)*time.Minute)
			if to == "" {
				to = overflowKeyID
			}
			f.decisions = append(f.decisions, model.Decision{
				At: at, Provider: "claude", Kind: model.DecisionFailover, SessionKey: b.SessionKey, Model: b.Model,
				ChosenAuthID: to, PreviousAuthID: a.AuthID, Note: failoverNote,
			})
			b.AuthID = to
			moved++
		}
	}
}

// logins is a pool with a seat in every login state but the network one:
// renewing, ageing, failing and two expired.
func (f *fixture) logins() {
	f.loginPool([]loginSeat{
		{seat: manySeat{id: "claude-eng-1.json", label: "eng-1 · healthy", name: "claude-eng-1.json", session: 0.31, weekly: 0.35, scoped: 0.28, elapsedDays: 2.5},
			team: "Acme Engineering 1", state: model.LoginOK, access: 6*time.Hour + 10*time.Minute},
		{seat: manySeat{id: "claude-eng-2.json", label: "eng-2 · login 28 days old", name: "claude-eng-2.json", session: 0.42, weekly: 0.51, scoped: 0.44, elapsedDays: 3.5},
			team: "Acme Engineering 2", state: model.LoginAgeing, access: 5*time.Hour + 2*time.Minute, loggedIn: 28*24*time.Hour - 3*time.Hour},
		{seat: manySeat{id: "claude-eng-3.json", label: "eng-3 · renewal failing", name: "claude-eng-3.json", session: 0.58, weekly: 0.62, scoped: 0.55, elapsedDays: 4.5},
			team: "Acme Engineering 3", state: model.LoginFailing, access: 2*time.Hour + 40*time.Minute},
		{seat: manySeat{id: "claude-eng-4.json", label: "eng-4 · login expired", name: "claude-eng-4.json", session: 0.20, weekly: 0.40, scoped: 0.30, elapsedDays: 1.5},
			team: "Acme Engineering 4", state: model.LoginExpired, access: -(1*time.Hour + 12*time.Minute)},
		{seat: manySeat{id: "claude-eng-5.json", label: "eng-5 · login expired", name: "claude-eng-5.json", session: 0.66, weekly: 0.71, scoped: 0.60, elapsedDays: 5.5},
			team: "Acme Engineering 5", state: model.LoginExpired, access: -25 * time.Minute},
		{seat: manySeat{id: "claude-eng-6.json", label: "eng-6 · healthy", name: "claude-eng-6.json", session: 0.12, weekly: 0.22, scoped: 0.18, elapsedDays: 0.9},
			team: "Acme Engineering 6", state: model.LoginOK, access: 7*time.Hour + 30*time.Minute},
	})
}

// loginsAll is a pool whose every login has expired: the host offers no seat,
// and every request lands on the credential in its next priority tier.
func (f *fixture) loginsAll() {
	seats := []loginSeat{}
	for i, access := range []time.Duration{-(3*time.Hour + 5*time.Minute), -(2*time.Hour + 20*time.Minute), -(55 * time.Minute), -(18 * time.Minute)} {
		n := string(rune('1' + i))
		seats = append(seats, loginSeat{
			seat: manySeat{id: "claude-eng-" + n + ".json", label: "eng-" + n + " · login expired", name: "claude-eng-" + n + ".json",
				session: 0.2 + 0.15*float64(i), weekly: 0.3 + 0.12*float64(i), scoped: 0.25 + 0.1*float64(i), elapsedDays: 1.5 + float64(i)},
			team: "Acme Engineering " + n, state: model.LoginExpired, access: access,
		})
	}
	f.loginPool(seats)
	// Behind the failovers, the run of requests the overflow key took.
	keys := []string{"5f2c0b7d4a19e83c", "a91d33e0c7b45f28", "c4408b1ef6d92a70", "77b0fe4c1a8d6392"}
	models := []string{modelFable, modelOpus, modelSonnet}
	for i := 0; i < 16; i++ {
		f.decisions = append(f.decisions, model.Decision{
			At: f.anchor.Add(-time.Duration(8+21*i) * time.Second), SessionKey: keys[i%len(keys)], Model: models[i%len(models)],
			Provider: "claude", ChosenAuthID: overflowKeyID, Kind: model.DecisionAffinityHit,
		})
	}
}

// loginsNetwork is a pool whose renewals are all overdue together, as while
// the host cannot reach Anthropic: every usage read fails too, and no login
// is at fault.
func (f *fixture) loginsNetwork() {
	seats := []loginSeat{}
	for i := 0; i < 4; i++ {
		n := string(rune('1' + i))
		seats = append(seats, loginSeat{
			seat: manySeat{id: "claude-eng-" + n + ".json", label: "eng-" + n + " · renewal overdue (network)", name: "claude-eng-" + n + ".json",
				session: 0.25 + 0.12*float64(i), weekly: 0.3 + 0.12*float64(i), scoped: 0.25 + 0.1*float64(i), elapsedDays: 1.5 + float64(i)},
			team: "Acme Engineering " + n, state: model.LoginStalled, access: 3*time.Hour + time.Duration(5+12*i)*time.Minute,
		})
	}
	f.loginPool(seats)
	for id, snap := range f.snapshots {
		snap.Err = "Get \"https://api.anthropic.com/api/oauth/usage\": dial tcp: lookup api.anthropic.com: no such host"
		snap.ErrCategory = "transport"
		f.snapshots[id] = snap
		f.observedAge[id] = 41 * time.Minute
	}
}

// stripLogins drops every login, which is the payload the status page reads
// from a plugin that does not report logins.
func (f *fixture) stripLogins() {
	for i := range f.auths {
		f.auths[i].Login = nil
	}
}
