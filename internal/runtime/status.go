package runtime

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/pace"
)

// DefaultStatusModel is the model the status view scores for before any
// request has been routed.
const DefaultStatusModel = "claude-opus-5"

// Status assembles the complete state the status app renders. modelID selects
// the model every credential is scored for; empty means the most recently
// routed model, or DefaultStatusModel before the first pick.
//
// The result carries labels and ids only. Nothing here reads a credential
// file, and the poller stores no token, so there is none to leak even though
// the resource route that serves this is unauthenticated.
//
// There is no warning for the host's own routing.session-affinity: the host
// stamps session_affinity_provider on every pick regardless of that setting
// (sdk/cliproxy/auth/conductor_selection.go:1474, :1730, :1790, :1902), so
// nothing the plugin receives distinguishes the two states.
func (p *Plugin) Status(now time.Time, modelID string) model.Status {
	cfg := p.Config()

	p.mu.Lock()
	info := model.PluginInfo{
		Name:              p.opts.Name,
		Version:           p.opts.Version,
		HostSchemaVersion: p.hostSchema,
		StartedAt:         p.startedAt,
	}
	auths := append([]HostAuthFileEntry(nil), p.auths...)
	polls := make(map[string]pollState, len(p.polls))
	for id, state := range p.polls {
		polls[id] = state
	}
	cache := make(map[string]model.CacheStats, len(p.cache))
	for id, stats := range p.cache {
		cache[id] = stats
	}
	lastModel := p.lastModel
	listErr := p.listErr
	single := make([]string, 0, len(p.singleWarned))
	for provider := range p.singleWarned {
		single = append(single, provider)
	}
	p.mu.Unlock()
	sort.Strings(single)

	if modelID == "" {
		modelID = lastModel
	}
	if modelID == "" {
		modelID = DefaultStatusModel
	}

	bindings := p.bindingStore()
	counts := bindings.CountByAuth()
	snapshots := make(map[string]model.AuthSnapshot)
	for _, snap := range p.quota.All() {
		snapshots[snap.AuthID] = snap
	}

	// One row per credential the host lists, plus any credential only a
	// snapshot or usage record knows about, so nothing routed disappears from
	// view between polls.
	entries := make(map[string]HostAuthFileEntry, len(auths))
	ids := make(map[string]struct{})
	for _, entry := range auths {
		id := authID(entry)
		entries[id] = entry
		ids[id] = struct{}{}
	}
	for id := range snapshots {
		ids[id] = struct{}{}
	}
	for id := range cache {
		ids[id] = struct{}{}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)

	rows := make([]model.AuthStatus, 0, len(ordered))
	for _, id := range ordered {
		entry, listed := entries[id]
		snap, hasSnap := snapshots[id]
		if !hasSnap {
			snap = model.AuthSnapshot{AuthID: id, Windows: []model.Window{}}
		}
		row := model.AuthStatus{
			AuthID:   id,
			Label:    snap.Label,
			Snapshot: snap,
			Score:    scoreWithStaleness(cfg, snap, hasSnap, id, modelID, now),
			Bindings: counts[id],
			Cache:    cache[id],
		}
		if listed {
			row.Label = authLabel(entry)
			row.Provider = strings.ToLower(entry.Provider)
			row.Priority = entry.Priority
			row.HostStatus = hostStatus(entry)
		}
		rows = append(rows, row)
	}

	warnings := make([]string, 0, 4)
	if !cfg.Enabled {
		warnings = append(warnings, "plugin is disabled by configuration; the host's own selector routes every request")
	}
	if listErr != "" {
		warnings = append(warnings, "credential listing is failing: "+listErr)
	}
	for _, provider := range single {
		warnings = append(warnings, fmt.Sprintf("provider %s offered a single candidate; spreading cannot work until every credential in the pool shares one priority value", provider))
	}
	for _, id := range ordered {
		entry, listed := entries[id]
		if !listed || entry.Disabled {
			continue
		}
		if state, ok := polls[id]; ok && state.err != "" {
			warnings = append(warnings, fmt.Sprintf("quota poll failing for %s (%s): %s", id, state.category, state.err))
		}
		if _, ok := snapshots[id]; !ok {
			warnings = append(warnings, fmt.Sprintf("no quota snapshot for %s; it is ineligible for cold picks", id))
		}
	}

	decisions := p.decisions.newestFirst()
	bound := bindings.All()
	if bound == nil {
		bound = []model.Binding{}
	}
	return model.Status{
		Now:       now,
		Plugin:    info,
		Config:    cfg,
		Model:     modelID,
		Auths:     rows,
		Bindings:  bound,
		Decisions: decisions,
		Warnings:  warnings,
	}
}

// scoreWithStaleness is the pace evaluation the pick would use for one
// credential, with the same staleness gate the pick applies.
func scoreWithStaleness(cfg model.Config, snap model.AuthSnapshot, hasSnap bool, id, modelID string, now time.Time) model.Score {
	switch {
	case !hasSnap:
		return model.Score{AuthID: id, Reason: model.ReasonNoSnapshot}
	case snap.Stale(now, cfg.Quota.MaxStaleness):
		return model.Score{AuthID: id, Reason: model.ReasonStale}
	}
	score := pace.ScoreAuth(cfg.Pace, snap, modelID, now)
	score.AuthID = id
	return score
}

// hostStatus is the credential state as the host reports it.
func hostStatus(entry HostAuthFileEntry) string {
	switch {
	case entry.Disabled:
		return "disabled"
	case entry.Unavailable:
		return "unavailable"
	case entry.Status != "":
		return entry.Status
	default:
		return "unknown"
	}
}
