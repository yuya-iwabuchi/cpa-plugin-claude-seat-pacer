package runtime

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/pace"
)

// pickEnvelope answers scheduler.pick. It never returns an error: an error
// envelope hard-fails the request with no fallback to the host's selector
// (sdk/cliproxy/auth/conductor_selection.go:807), so even an undecodable
// payload declines.
func (p *Plugin) pickEnvelope(payload []byte) ([]byte, error) {
	var req SchedulerPickRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return okEnvelope(SchedulerPickResponse{Handled: false})
	}
	return okEnvelope(p.pick(req))
}

// pickInput is the request after provider resolution and candidate filtering.
type pickInput struct {
	req        SchedulerPickRequest
	cfg        model.Config
	now        time.Time
	provider   string
	candidates []string
	identity   bridgeIdentity
	// retryOf names the credential that already failed this request, present
	// only on a retry pick.
	retryOf string
	snaps   map[string]model.AuthSnapshot
	scores  []model.Score
}

func (in pickInput) isCandidate(authID string) bool {
	for _, id := range in.candidates {
		if id == authID {
			return true
		}
	}
	return false
}

func (in pickInput) scoreOf(authID string) model.Score {
	for _, s := range in.scores {
		if s.AuthID == authID {
			return s
		}
	}
	return model.Score{AuthID: authID, Reason: model.ReasonNotCandidate}
}

// hasAlternativeHome reports whether some candidate other than authID is
// worth moving to: one the pace curve rates eligible, or one whose state
// leaves open that it can serve. A candidate the provider has already rejected
// for this model is neither.
func (in pickInput) hasAlternativeHome(authID string) bool {
	for _, id := range in.candidates {
		if id == authID {
			continue
		}
		if in.scoreOf(id).Eligible {
			return true
		}
		if snap, ok := in.snaps[id]; !ok || !blockedFor(snap, in.req.Model) {
			return true
		}
	}
	return false
}

// pick is the routing decision. It performs in-memory reads only: the host
// gives it no timeout, so any blocking call here would park a goroutine for
// good and block dlclose.
//
// Rules, in order:
//
//  1. Decline (Handled false) when the plugin is disabled, no governed
//     provider is on the route, the model is not governed, no candidate
//     remains, or there is neither a session key nor a usable snapshot.
//  2. With a session key and affinity on: a subagent inherits its parent's
//     credential when that credential is a candidate and its snapshot shows no
//     window the provider already rejected for this model; otherwise the
//     session's own binding is honoured under those same two conditions, even
//     when its pace score trails (OverrideThreshold), unless OverrideThreshold
//     is off and the pace winner beats it by the hysteresis margin. A binding
//     that fails any of those fails over to the cold pick, except that a
//     rejected binding stands when every other candidate is rejected too.
//  3. Cold pick: pace.Rank over the candidates with stale snapshots marked
//     ineligible; the pace winner takes the session. When nothing is eligible
//     but a session key exists, the candidate with the fewest live bindings
//     (lowest id on a tie) takes it, so the conversation still gets one
//     stable home.
func (p *Plugin) pick(req SchedulerPickRequest) SchedulerPickResponse {
	cfg := p.config()
	now := p.now()
	in := pickInput{req: req, cfg: cfg, now: now, identity: readBridge(req.Options.Headers)}
	in.retryOf = metadataString(req.Options.Metadata, MetadataSelectedAuthID)

	decline := func(note string) SchedulerPickResponse {
		p.record(model.Decision{
			At:         now,
			SessionKey: in.identity.key,
			Model:      req.Model,
			Provider:   in.provider,
			Kind:       model.DecisionDeclined,
			Note:       note,
			Subagent:   in.identity.subagent,
		})
		return SchedulerPickResponse{Handled: false}
	}

	if !cfg.Enabled {
		return decline("plugin disabled")
	}
	in.provider = governedProvider(cfg, req)
	if in.provider == "" {
		return decline("no governed provider on this route")
	}
	if !cfg.GovernsModel(req.Model) {
		return decline("model not governed")
	}
	for _, c := range req.Candidates {
		if strings.EqualFold(c.Provider, in.provider) && c.ID != "" && c.ID != in.retryOf {
			in.candidates = append(in.candidates, c.ID)
		}
	}
	if len(in.candidates) == 0 {
		return decline("no candidates for " + in.provider)
	}
	// A retry pick has the failed credential removed and a pinned request is
	// offered exactly one candidate by design, so neither says anything about
	// the pool's priority values.
	if _, pinned := req.Options.Metadata[MetadataPinnedAuthID]; !pinned && in.retryOf == "" {
		p.observeCandidates(in.provider, len(in.candidates))
	}

	in.snaps = make(map[string]model.AuthSnapshot, len(in.candidates))
	usable := false
	for _, id := range in.candidates {
		snap, ok := p.quota.Get(id)
		if !ok {
			continue
		}
		in.snaps[id] = snap
		if !snap.Stale(now, cfg.Quota.MaxStaleness) {
			usable = true
		}
	}
	if in.identity.key == "" && !usable {
		return decline("no session key and no usable quota snapshot")
	}
	in.scores = rankWithStaleness(cfg, in.snaps, in.candidates, req.Model, now)

	if cfg.Affinity.Enabled && in.identity.key != "" {
		if resp, ok := p.pickByAffinity(in); ok {
			return resp
		}
	}
	return p.pickCold(in, "", "")
}

// pickByAffinity applies rule 2. It reports false when no binding decides the
// pick and the caller must fall through to a cold pick.
func (p *Plugin) pickByAffinity(in pickInput) (SchedulerPickResponse, bool) {
	bindings := p.bindingStore()
	id := in.identity

	if in.cfg.Affinity.Subagents && id.subagent && id.parent != "" {
		parent, ok := bindings.Lookup(in.provider, in.req.Model, id.parent, in.now)
		// A credential the provider has already rejected is no better a home
		// for the child than for the parent.
		if ok && in.isCandidate(parent.AuthID) && !blockedFor(in.snaps[parent.AuthID], in.req.Model) {
			bindings.Bind(in.provider, in.req.Model, id.key, parent.AuthID, in.now)
			return p.decide(in, model.Decision{
				ChosenAuthID: parent.AuthID,
				Kind:         model.DecisionAffinityHit,
				Note:         "pinned to parent session",
			}), true
		}
	}

	bound, ok := bindings.Lookup(in.provider, in.req.Model, id.key, in.now)
	if !ok {
		return SchedulerPickResponse{}, false
	}
	if !in.isCandidate(bound.AuthID) {
		note := "bound credential not offered"
		if bound.AuthID == in.retryOf {
			note = "bound credential failed this request"
		}
		return p.pickCold(in, bound.AuthID, note), true
	}

	// A window the provider has already rejected for this model makes the
	// binding no home at all, whether or not another candidate outscores it: a
	// host allowed one pick per request has no retry to recover on. With every
	// other candidate rejected too the move buys nothing and costs a cross-org
	// cache miss, and the fewest-conversations fallback alternates seats
	// request by request, so the
	// binding stands until somewhere better exists.
	if blockedFor(in.snaps[bound.AuthID], in.req.Model) {
		if !in.hasAlternativeHome(bound.AuthID) {
			return p.decide(in, model.Decision{
				ChosenAuthID: bound.AuthID,
				Kind:         model.DecisionAffinityHit,
				Note:         "binding kept; every seat is rate-limited for this model",
				Scores:       in.scores,
			}), true
		}
		return p.pickCold(in, bound.AuthID, "bound credential is rate-limited for this model"), true
	}
	best, hasBest := pace.Best(in.scores)
	if hasBest && best.AuthID != bound.AuthID && !in.cfg.Affinity.OverrideThreshold &&
		pace.ShouldSwitch(in.cfg.Pace, in.scoreOf(bound.AuthID), best) {
		return p.pickCold(in, bound.AuthID, "pace winner beats the binding by the hysteresis margin"), true
	}
	return p.decide(in, model.Decision{
		ChosenAuthID: bound.AuthID,
		Kind:         model.DecisionAffinityHit,
	}), true
}

// pickCold applies rule 3. previous names the credential a binding is moving
// away from, which turns the decision into a failover.
func (p *Plugin) pickCold(in pickInput, previous, note string) SchedulerPickResponse {
	chosen := ""
	if best, ok := pace.Best(in.scores); ok {
		chosen = best.AuthID
	} else {
		if in.identity.key == "" {
			return p.declineWithScores(in, "no eligible candidate")
		}
		chosen = leastBound(in.candidates, p.bindingStore().CountByAuth())
		note = joinNotes(note, "no eligible candidate; the seat with the fewest live conversations takes it")
	}

	d := model.Decision{ChosenAuthID: chosen, Kind: model.DecisionColdPick, Note: note, Scores: in.scores}
	if previous != "" {
		d.Kind = model.DecisionFailover
		d.PreviousAuthID = previous
	}
	if in.identity.key != "" && in.cfg.Affinity.Enabled {
		p.bindingStore().Bind(in.provider, in.req.Model, in.identity.key, chosen, in.now)
	} else if in.identity.key == "" {
		d.Note = joinNotes(d.Note, "no session key; not pinned")
	}
	return p.decide(in, d)
}

func (p *Plugin) declineWithScores(in pickInput, note string) SchedulerPickResponse {
	p.record(model.Decision{
		At:         in.now,
		SessionKey: in.identity.key,
		Model:      in.req.Model,
		Provider:   in.provider,
		Kind:       model.DecisionDeclined,
		Note:       note,
		Scores:     in.scores,
		Subagent:   in.identity.subagent,
	})
	return SchedulerPickResponse{Handled: false}
}

// decide fills the request-derived fields of a decision, records it and
// answers the host with the chosen credential.
func (p *Plugin) decide(in pickInput, d model.Decision) SchedulerPickResponse {
	d.At = in.now
	d.SessionKey = in.identity.key
	d.Model = in.req.Model
	d.Provider = in.provider
	d.Subagent = in.identity.subagent
	if in.retryOf != "" {
		d.Note = joinNotes(d.Note, "retry after "+in.retryOf)
	}
	p.record(d)
	return SchedulerPickResponse{Handled: true, AuthID: d.ChosenAuthID}
}

// governedProvider resolves the provider this pick is for: the primary
// provider when the plugin governs it, else the first governed entry of the
// route's provider list, else the first governed candidate. An inbound
// /v1/messages is a mixed route with an empty primary provider, so the list
// is the usual source.
func governedProvider(cfg model.Config, req SchedulerPickRequest) string {
	if req.Provider != "" && cfg.GovernsProvider(strings.ToLower(req.Provider)) {
		return strings.ToLower(req.Provider)
	}
	for _, provider := range req.Providers {
		if cfg.GovernsProvider(strings.ToLower(provider)) {
			return strings.ToLower(provider)
		}
	}
	for _, c := range req.Candidates {
		if cfg.GovernsProvider(strings.ToLower(c.Provider)) {
			return strings.ToLower(c.Provider)
		}
	}
	return ""
}

// rankWithStaleness scores the candidates with a stale snapshot treated as no
// snapshot, then names the reason so the status view distinguishes "never
// read" from "read too long ago".
func rankWithStaleness(cfg model.Config, snaps map[string]model.AuthSnapshot, candidates []string, modelID string, now time.Time) []model.Score {
	fresh := make(map[string]model.AuthSnapshot, len(snaps))
	stale := make(map[string]bool)
	for id, snap := range snaps {
		if snap.Stale(now, cfg.Quota.MaxStaleness) {
			stale[id] = true
			continue
		}
		fresh[id] = snap
	}
	scores := pace.Rank(cfg.Pace, fresh, candidates, modelID, now)
	for i := range scores {
		if stale[scores[i].AuthID] {
			scores[i].Reason = model.ReasonStale
		}
	}
	return scores
}

// blockedFor reports whether the provider has already refused a window that
// bears on the requested model.
func blockedFor(snap model.AuthSnapshot, modelID string) bool {
	family := model.FamilyOf(modelID)
	for _, w := range snap.Windows {
		if w.Blocking() && w.BearsOn(family) {
			return true
		}
	}
	return false
}

// leastBound picks the candidate carrying the fewest live bindings, lowest id
// on a tie, so repeated calls on the same state agree.
func leastBound(candidates []string, counts map[string]int) string {
	ids := append([]string(nil), candidates...)
	sort.Strings(ids)
	chosen := ids[0]
	for _, id := range ids[1:] {
		if counts[id] < counts[chosen] {
			chosen = id
		}
	}
	return chosen
}

func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// observeCandidates records how many candidates a provider was last offered,
// on an affinity hit as much as on a cold pick, because the count describes
// the pool rather than the decision. A count of one means the host capped the
// pool at a single priority tier and spreading cannot work; the status view
// warns for as long as that holds, and the poller carries the log line.
func (p *Plugin) observeCandidates(provider string, count int) {
	p.mu.Lock()
	p.singleCandidates[provider] = count
	p.mu.Unlock()
}
