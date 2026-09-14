package runtime

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/pace"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// defaultStatusModel is the model the status view scores for before any
// request has been routed.
const defaultStatusModel = "claude-opus-5"

// Status assembles the complete state the status app renders. modelID selects
// the model every credential is scored for; empty means the most recently
// routed model, or defaultStatusModel before the first pick.
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
	cfg := p.config()

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
	polledAt, nextPollAt := p.polledAt, p.nextPollAt
	listErr := p.listErr
	// Only providers whose latest pick was still down to one candidate, so the
	// warning clears once the pool's priority values are fixed.
	single := make([]string, 0, len(p.singleCandidates))
	for provider, count := range p.singleCandidates {
		if count == 1 {
			single = append(single, provider)
		}
	}
	p.mu.Unlock()
	sort.Strings(single)

	if modelID == "" {
		modelID = lastModel
	}
	if modelID == "" {
		modelID = defaultStatusModel
	}

	bindings := p.bindingStore()
	counts := bindings.CountByAuth()
	snapshots := make(map[string]model.AuthSnapshot)
	for _, snap := range p.quota.All() {
		snapshots[snap.AuthID] = snap
	}

	// One row per credential the host lists, plus any the quota store holds a
	// reading for, so a credential the host stops listing keeps its row until
	// the next poll prunes it. Cache counters never open a row: a usage record
	// carries an id and nothing else, so a row from one alone would name no
	// label, provider, priority or status.
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
			Score:    pace.ScoreWithStaleness(cfg, snap, hasSnap, id, modelID, now),
			Bindings: counts[id],
			Cache:    cache[id],
			History:  p.quota.History(id, quota.HistoryPublishMax),
		}
		if row.History == nil {
			row.History = []model.WindowHistory{}
		}
		if listed {
			row.Label = authLabel(entry)
			row.Name = entry.Name
			row.Email = entry.Email
			row.Provider = strings.ToLower(entry.Provider)
			row.Priority = entry.Priority
			row.HostStatus = hostStatus(entry)
		}
		rows = append(rows, row)
	}

	seats := make(map[string]SeatWarningState, len(ordered))
	for _, id := range ordered {
		entry, listed := entries[id]
		_, hasSnap := snapshots[id]
		state := SeatWarningState{Listed: listed, Disabled: listed && entry.Disabled, HasSnapshot: hasSnap}
		if poll, ok := polls[id]; ok {
			state.PollErr, state.PollCategory = poll.err, poll.category
		}
		seats[id] = state
	}
	warnings := Warnings(cfg.Enabled, listErr, single, rows, seats)

	decisions := p.decisions.newestFirst()
	bound := bindings.All()
	if bound == nil {
		bound = []model.Binding{}
	}
	return model.Status{
		Now:        now,
		Plugin:     info,
		Config:     cfg,
		Model:      modelID,
		PolledAt:   polledAt,
		NextPollAt: nextPollAt,
		Auths:      rows,
		Bindings:   bound,
		Decisions:  decisions,
		Warnings:   warnings,
	}
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

// singleCandidateWarning names why a provider was offered one candidate. The
// host filters the list before the plugin sees it and caps it at the highest
// priority tier, so a lone candidate means the pool holds one credential, the
// rest sit on a lower tier, or the rest are unavailable. Only the tier case is
// a misconfiguration, and a pool the plugin cannot see claims no cause at all.
func singleCandidateWarning(provider string, rows []model.AuthStatus) string {
	tiers := make(map[int]struct{}, len(rows))
	pool := 0
	for _, row := range rows {
		if row.Provider != provider {
			continue
		}
		pool++
		tiers[row.Priority] = struct{}{}
	}
	switch {
	case pool == 0:
		return fmt.Sprintf("provider %s offered a single candidate; the host filters candidates before the plugin sees them, so the rest are unavailable or on a lower priority tier", provider)
	case pool == 1:
		return fmt.Sprintf("provider %s offered a single candidate, which is the only credential in the pool", provider)
	case len(tiers) > 1:
		return fmt.Sprintf("provider %s offered a single candidate; the host caps candidates at the highest priority tier, so every credential in the pool needs the same priority value", provider)
	default:
		return fmt.Sprintf("provider %s offered a single candidate; the pool shares one priority tier, so the rest are unavailable to the host or already rejected upstream", provider)
	}
}

// SeatWarningState is what Warnings needs about one credential beyond its
// status row: whether the host still lists it, whether a reading exists for
// it, and the outcome of its last poll.
type SeatWarningState struct {
	Listed       bool
	Disabled     bool
	HasSnapshot  bool
	PollErr      string
	PollCategory string
}

// Warnings is the operator warning list a status view carries: the plugin
// being off, a failing credential listing, a provider the host offered one
// candidate for, and per credential a failing poll or a missing reading. A
// credential the host no longer lists, or has disabled, warns about neither:
// the poller skips it, so it holds no reading by design.
//
// Order follows rows, so the same state renders the same list twice.
func Warnings(enabled bool, listErr string, singleCandidateProviders []string, rows []model.AuthStatus, seats map[string]SeatWarningState) []string {
	warnings := make([]string, 0, 4)
	if !enabled {
		warnings = append(warnings, "plugin is disabled by configuration; the host's own selector routes every request")
	}
	if listErr != "" {
		warnings = append(warnings, "credential listing is failing: "+listErr)
	}
	for _, provider := range singleCandidateProviders {
		warnings = append(warnings, singleCandidateWarning(provider, rows))
	}
	for _, row := range rows {
		seat := seats[row.AuthID]
		if !seat.Listed || seat.Disabled {
			continue
		}
		if seat.PollErr != "" {
			warnings = append(warnings, fmt.Sprintf("quota poll failing for %s (%s): %s", row.AuthID, seat.PollCategory, seat.PollErr))
		}
		if !seat.HasSnapshot {
			warnings = append(warnings, fmt.Sprintf("no quota snapshot for %s; it cannot take a new conversation", row.AuthID))
		}
	}
	return warnings
}
