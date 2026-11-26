package runtime

import (
	"context"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// holdMargin is how far past a refusal span's end the host's cooldown must
// run to count as stale. A span that runs to its reset ends where the
// cooldown the 429 set does.
const holdMargin = 10 * time.Minute

// deadlineSlack is how far the host's cooldown may end from the reset a
// refusal span expected and still be the cooldown that refusal set. The host
// cools a credential until the latest reset its 429 names, plus up to 30
// seconds (internal/runtime/executor/helps/claude_ratelimit.go in v8.0.12);
// the rest is headroom for a span whose reset came from a usage read rather
// than from the 429 itself.
const deadlineSlack = 10 * time.Minute

// repeatWindow is how long after the plugin asks for a reset a new refusal
// span, cooled to the same deadline, still counts as that cooldown returning:
// a seat whose quota was not really back is refused on its first requests,
// while refilling a window it got back takes far longer.
const repeatWindow = time.Hour

// errResetUnsupported is the host's answer to a callback it does not have.
// Source: internal/pluginhost/host_callbacks.go:131.
const errResetUnsupported = "unsupported host callback"

// staleHold is a host cooldown on a credential whose quota the provider has
// already given back.
type staleHold struct {
	// until is when the host's cooldown ends.
	until time.Time
	// released is when the quota came back: the cycle the cause's window
	// opened at or after the refusal span that set the cooldown ended.
	released time.Time
	// span is that span's start.
	span int64
	// asked is when the plugin asked the host to clear it; zero until then.
	asked time.Time
	// problem says why the cooldown still stands. On a reset the plugin
	// asked for, it is empty when the host accepted it.
	problem string
}

// capsEveryRequest reports whether a window caps requests for every model. A
// refusal on one of these cools the whole credential unless
// upstream.claude.model-level-cooling is on, and it is the only refusal
// findStaleHold ties a cooldown to. Whether a family cap's refusal cools the
// whole credential too depends on the 429's headers
// (internal/runtime/executor/helps/claude_ratelimit.go in v8.0.12).
func capsEveryRequest(kind model.WindowKind) bool {
	return kind == model.WindowSession || kind == model.WindowWeekly
}

// within reports whether the host's cooldown ends within deadlineSlack of a
// span's expected reset.
func within(until time.Time, l model.Lock) bool {
	return until.Sub(time.Unix(l.ExpectedEnd(), 0)).Abs() <= deadlineSlack
}

// reopened reports whether a window reopened after a span ended: its history
// opened a cycle at or after the span's end, as the provider clearing the
// window or its rolling onto a new reset does, or the span ended as cleared
// on a window snap no longer carries, as a usage read that stops reporting
// the window ends it. A span a served request ended shows only that one
// request got through.
func reopened(snap model.AuthSnapshot, h model.WindowHistory, l model.Lock) bool {
	_, ok := reopenedAt(snap, h, l)
	return ok
}

// reopenedAt is when the window reopened says it did.
func reopenedAt(snap model.AuthSnapshot, h model.WindowHistory, l model.Lock) (time.Time, bool) {
	for _, c := range h.Cycles {
		if len(c.Samples) > 0 && c.Samples[0].At.Unix() >= l.To {
			return c.Samples[0].At, true
		}
	}
	if _, carried := snap.Window(h.Kind, h.Scope); !carried && l.End == model.LockEndCleared {
		return time.Unix(l.To, 0), true
	}
	return time.Time{}, false
}

// explains reports whether a cooldown ending at until is the one the 429
// that opened span c set: the host cools until the latest reset that 429
// names, so a span on any window, a family cap's included, that was open
// when c began and expected that reset accounts for it.
func explains(history []model.WindowHistory, c model.Lock, until time.Time) bool {
	for _, h := range history {
		for _, l := range h.Locks {
			if l.From <= c.From && (l.End == "" || l.To >= c.From) && within(until, l) {
				return true
			}
		}
	}
	return false
}

// findStaleHold reports a host cooldown that outlasts the refusal it came
// from. Every condition has to hold:
//
//   - The host lists the credential as unavailable with a cooldown still
//     ahead.
//   - The usage read this poll took shows quota left in each of the 5-hour
//     and weekly windows it carries, carries at least one, and shows the
//     provider refusing neither.
//   - No refusal span on those windows is ongoing. The cause is the latest of
//     them to end whose window has cleared or rolled since, or that the read
//     no longer reports, and which `explains` ties to the cooldown.
//   - None of them still open when the cause began expected the reset the
//     cooldown ends at without its window clearing or rolling since: while
//     that window stays full the provider refuses every model.
//   - That span ended more than holdMargin before the cooldown does, and
//     before the read.
//   - No family cap's span began after it ended and expected the reset the
//     cooldown ends at: a later refusal of that cap alone can cool the whole
//     credential to the cap's reset, as capsEveryRequest describes. A later
//     5-hour or weekly span either is ongoing, has not reopened, or is the
//     cause.
//
// A family cap still full does not hold the reset back: the host's reset
// lifts that family's cooldown too, but the pick sends the family nowhere
// full while another seat has room.
func findStaleHold(entry HostAuthFileEntry, snap model.AuthSnapshot, history []model.WindowHistory, now time.Time) (staleHold, bool) {
	if entry.Disabled || !entry.Unavailable || entry.AuthIndex == "" || !entry.NextRetryAfter.After(now) {
		return staleHold{}, false
	}
	read := false
	for _, w := range snap.Windows {
		if !capsEveryRequest(w.Kind) {
			continue
		}
		if w.Blocking() || w.Utilization >= 1 {
			return staleHold{}, false
		}
		read = true
	}
	if !read {
		return staleHold{}, false
	}
	var cause model.Lock
	var released time.Time
	for _, h := range history {
		if !capsEveryRequest(h.Kind) {
			continue
		}
		for _, l := range h.Locks {
			if l.End == "" {
				return staleHold{}, false
			}
			if at, ok := reopenedAt(snap, h, l); ok && l.To > cause.To && explains(history, l, entry.NextRetryAfter) {
				cause, released = l, at
			}
		}
	}
	if cause.End == "" {
		return staleHold{}, false
	}
	for _, h := range history {
		if !capsEveryRequest(h.Kind) {
			continue
		}
		for _, l := range h.Locks {
			if l.To >= cause.From && within(entry.NextRetryAfter, l) && !reopened(snap, h, l) {
				return staleHold{}, false
			}
		}
	}
	for _, h := range history {
		if capsEveryRequest(h.Kind) {
			continue
		}
		for _, l := range h.Locks {
			if l.From >= cause.To && within(entry.NextRetryAfter, l) {
				return staleHold{}, false
			}
		}
	}
	ended := time.Unix(cause.To, 0)
	if !snap.ObservedAt.After(ended) || entry.NextRetryAfter.Sub(ended) <= holdMargin {
		return staleHold{}, false
	}
	return staleHold{until: entry.NextRetryAfter, released: released, span: cause.From}, true
}

// releaseHolds asks the host to clear each stale cooldown among the
// credentials the poll read, once per refusal span: a reset that failed, or
// one followed within repeatWindow by a refusal the host cools to the same
// deadline, is left to the operator rather than retried every poll. A host
// without the callback is asked once per process. The cooldowns left
// standing are what the status view warns about.
//
// It runs on the poll path, never on the pick path, and holds mu only for
// field access.
func (p *Plugin) releaseHolds(ctx context.Context, entries []HostAuthFileEntry, read map[string]bool) {
	now := p.now()
	held := make(map[string]staleHold)
	cleared := make(map[string]bool)
	listed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id := authID(entry)
		listed[id] = true
		if !read[id] {
			continue
		}
		switch hold, state := p.settleHold(ctx, id, entry, now); state {
		case holdHeld:
			held[id] = hold
		case holdCleared:
			cleared[id] = true
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.holdResets {
		if !listed[id] {
			delete(p.holdResets, id)
		}
	}
	p.holds = held
	// p.auths is the listing this poll read before the resets, and it stands
	// until the next poll. A cleared entry takes the state the host's reset
	// leaves on a credential with no other fault.
	for i := range p.auths {
		if cleared[authID(p.auths[i])] {
			markCleared(&p.auths[i])
		}
	}
}

// releaseHold is releaseHolds for one seat read outside a poll, as after a
// reset claim. It replaces that seat's hold and listing entry and leaves
// every other seat's as the last poll left them.
func (p *Plugin) releaseHold(ctx context.Context, entry HostAuthFileEntry) {
	id := authID(entry)
	hold, state := p.settleHold(ctx, id, entry, p.now())
	p.mu.Lock()
	defer p.mu.Unlock()
	// status reads p.holds after it lets go of mu, so the map is replaced
	// rather than written in place.
	holds := make(map[string]staleHold, len(p.holds)+1)
	for k, v := range p.holds {
		if k != id {
			holds[k] = v
		}
	}
	if state == holdHeld {
		holds[id] = hold
	}
	p.holds = holds
	if state == holdCleared {
		p.auths = withEntry(p.auths, id, markCleared)
	}
}

// markCleared gives a listing entry the state the host's reset leaves on a
// credential with no other fault.
func markCleared(entry *HostAuthFileEntry) {
	entry.Unavailable, entry.NextRetryAfter = false, time.Time{}
	entry.Status, entry.StatusMessage = "active", ""
}

// What settleHold did about one seat's cooldown.
const (
	holdNone    = iota // no stale cooldown, or no time left to ask about one
	holdHeld           // a stale cooldown still stands
	holdCleared        // the host cleared a stale cooldown
)

// settleHold looks for a stale host cooldown on one seat with a fresh usage
// read, and asks the host to clear one it finds unless releaseHolds' rules
// leave it to the operator. A hold it reports carries why it still stands.
func (p *Plugin) settleHold(ctx context.Context, id string, entry HostAuthFileEntry, now time.Time) (staleHold, int) {
	snap, _ := p.quota.Get(id)
	// One sample per window is enough: only the refusal spans are read, and
	// History copies every one of them.
	hold, ok := findStaleHold(entry, snap, p.quota.History(id, 1, now), now)
	if !ok {
		return staleHold{}, holdNone
	}
	p.mu.Lock()
	prior, tried := p.holdResets[id]
	unsupported := p.resetUnsupported
	p.mu.Unlock()
	switch {
	case tried && (prior.span == hold.span ||
		hold.until.Sub(prior.until).Abs() <= deadlineSlack && time.Unix(hold.span, 0).Sub(prior.asked) < repeatWindow):
		hold.problem = prior.problem
		if hold.problem == "" {
			hold.problem = "the host cooled it again after the plugin cleared it"
		}
	case unsupported:
		hold.problem = resetUnsupportedProblem
	case ctx.Err() != nil:
		return staleHold{}, holdNone
	default:
		hold.problem = p.resetHold(ctx, id, entry.AuthIndex, hold)
		if hold.problem == "" {
			return staleHold{}, holdCleared
		}
	}
	return hold, holdHeld
}

// resetUnsupportedProblem is the warning's reason for a host older than the
// reset callback.
const resetUnsupportedProblem = "this host cannot clear a cooldown for a plugin (host.routing.reset_cooldown needs CLIProxyAPI 8.0.12 or newer), " +
	"so reset it with the management API's reset-quota or restart the host"

// resetHold asks the host to clear one stale cooldown and records the
// attempt. It returns why the cooldown still stands, or empty when the host
// accepted the reset.
func (p *Plugin) resetHold(ctx context.Context, id, authIndex string, hold staleHold) string {
	fields := map[string]any{
		"auth_id":    id,
		"released":   hold.released.UTC().Format(time.RFC3339),
		"held_until": hold.until.UTC().Format(time.RFC3339),
	}
	err := p.host.resetCooldown(ctx, authIndex)
	problem := ""
	switch {
	case err == nil:
		p.host.log("info", "claude-seat-pacer cleared a host cooldown on a seat whose quota is back", fields)
	case strings.Contains(err.Error(), errResetUnsupported):
		problem = resetUnsupportedProblem
		p.host.log("warn", "claude-seat-pacer: this host has no host.routing.reset_cooldown; a seat whose quota is back stays cooled", fields)
		p.mu.Lock()
		p.resetUnsupported = true
		p.mu.Unlock()
		return problem
	case ctx.Err() != nil:
		// The host may yet act on the call; the next poll looks again.
		return "the host did not answer the reset in time"
	default:
		problem = "clearing the cooldown failed: " + err.Error()
		fields["error"] = err.Error()
		p.host.log("warn", "claude-seat-pacer could not clear a host cooldown on a seat whose quota is back", fields)
	}
	p.mu.Lock()
	hold.problem, hold.asked = problem, p.now()
	p.holdResets[id] = hold
	p.mu.Unlock()
	return problem
}
