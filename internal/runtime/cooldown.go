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
	// released is when the refusal span that set it ended.
	released time.Time
	// span is that span's start.
	span int64
	// asked is when the plugin asked the host to clear it; zero until then.
	asked time.Time
	// problem says why the cooldown still stands. On a reset the plugin
	// asked for, it is empty when the host accepted it.
	problem string
}

// capsEveryRequest reports whether a window caps requests for every model.
// Only a refusal on one of these is sure to cool the whole credential, unless
// upstream.claude.model-level-cooling is on: the host cools only the refused
// model for a family cap's refusal while the 5-hour and weekly windows report
// allowed.
func capsEveryRequest(kind model.WindowKind) bool {
	return kind == model.WindowSession || kind == model.WindowWeekly
}

// within reports whether the host's cooldown ends within deadlineSlack of a
// span's expected reset.
func within(until time.Time, l model.Lock) bool {
	return until.Sub(time.Unix(l.ExpectedEnd(), 0)).Abs() <= deadlineSlack
}

// reopened reports whether a window's history opened a cycle at or after a
// span ended: the provider cleared the window or it rolled onto a new reset.
// A span a served request ended shows only that one request got through.
func reopened(h model.WindowHistory, l model.Lock) bool {
	for _, c := range h.Cycles {
		if len(c.Samples) > 0 && c.Samples[0].At.Unix() >= l.To {
			return true
		}
	}
	return false
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
//     them to end whose window has cleared or rolled since, and which
//     explains ties to the cooldown.
//   - None of them still open when the cause began expected the reset the
//     cooldown ends at without its window clearing or rolling since: while
//     that window stays full the provider refuses every model.
//   - That span ended more than holdMargin before the cooldown does, and
//     before the read.
//   - No span on any window, a family cap's included, began after it ended
//     and expected the reset the cooldown ends at: a later refusal explains
//     the cooldown better, as when a family cap's refusal cooled the whole
//     credential.
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
	for _, h := range history {
		if !capsEveryRequest(h.Kind) {
			continue
		}
		for _, l := range h.Locks {
			if l.End == "" {
				return staleHold{}, false
			}
			if l.To > cause.To && reopened(h, l) && explains(history, l, entry.NextRetryAfter) {
				cause = l
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
			if l.To >= cause.From && within(entry.NextRetryAfter, l) && !reopened(h, l) {
				return staleHold{}, false
			}
		}
	}
	for _, h := range history {
		for _, l := range h.Locks {
			if l.From >= cause.To && within(entry.NextRetryAfter, l) {
				return staleHold{}, false
			}
		}
	}
	released := time.Unix(cause.To, 0).UTC()
	if !snap.ObservedAt.After(released) || entry.NextRetryAfter.Sub(released) <= holdMargin {
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
	listed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id := authID(entry)
		listed[id] = true
		if !read[id] {
			continue
		}
		snap, _ := p.quota.Get(id)
		// One sample per window is enough: only the refusal spans are read,
		// and History copies every one of them.
		hold, ok := findStaleHold(entry, snap, p.quota.History(id, 1, now), now)
		if !ok {
			continue
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
			continue
		default:
			hold.problem = p.resetHold(ctx, id, entry.AuthIndex, hold)
			if hold.problem == "" {
				continue
			}
		}
		held[id] = hold
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.holdResets {
		if !listed[id] {
			delete(p.holdResets, id)
		}
	}
	p.holds = held
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
		"released":   hold.released.Format(time.RFC3339),
		"held_until": hold.until.Format(time.RFC3339),
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
