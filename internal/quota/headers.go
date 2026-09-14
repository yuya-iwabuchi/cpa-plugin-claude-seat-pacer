package quota

import (
	"strconv"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/httpx"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// Unified rate-limit header names. The unsuffixed triple reports the
// account-level verdict and names the window that produced it; each suffixed
// family reports one window.
const (
	headerPrefix = "anthropic-ratelimit-unified-"

	headerStatus = headerPrefix + "status"
	headerReset  = headerPrefix + "reset"
	headerClaim  = headerPrefix + "representative-claim"

	suffixStatus      = "-status"
	suffixUtilization = "-utilization"
	suffixReset       = "-reset"
)

// headerWindow binds one suffixed header family to a window identity. 7d_oi is
// seven_day_overage_included, which in 2026 is the Fable cap.
type headerWindow struct {
	suffix   string
	kind     model.WindowKind
	scope    string
	duration time.Duration
}

var headerWindows = []headerWindow{
	{suffix: "5h", kind: model.WindowSession, duration: model.SessionDuration},
	{suffix: "7d", kind: model.WindowWeekly, duration: model.WeeklyDuration},
	{suffix: "7d_oi", kind: model.WindowWeeklyScoped, scope: model.FamilyFable, duration: model.WeeklyDuration},
}

// accountClaims maps the Representative-Claim values that name no model family
// onto the window each of them describes. seven_day_overage_included is
// accepted alongside the documented values because it is the 7d_oi window's own
// name.
var accountClaims = map[string]windowKey{
	"five_hour":                  {kind: model.WindowSession},
	"seven_day":                  {kind: model.WindowWeekly},
	"seven_day_overage_included": {kind: model.WindowWeeklyScoped, scope: model.FamilyFable},
}

// claimWindow reports the window identity a Representative-Claim names, and
// false for a claim naming neither an account-level window nor a model family.
func claimWindow(claim string) (windowKey, bool) {
	if key, ok := accountClaims[claim]; ok {
		return key, true
	}
	if family := model.FamilyOf(claim); family != "" {
		return windowKey{kind: model.WindowWeeklyScoped, scope: family}, true
	}
	return windowKey{}, false
}

// ParseResponseHeaders reads the unified rate-limit headers off a real
// response. Header lookup is case-insensitive, and header utilization is
// carried through unscaled.
//
// Only windows that traffic has actually touched appear here, which is why a
// header read merges into a snapshot through Store.MergeHeaders instead of
// replacing it. A window whose utilization is absent or unparsable is left
// out. No window carries a severity: the headers report none.
func ParseResponseHeaders(h map[string][]string, now time.Time) []model.Window {
	get := httpx.NewIndex(h).Lookup
	set := newWindowSet()

	for _, hw := range headerWindows {
		raw, ok := get(headerPrefix + hw.suffix + suffixUtilization)
		if !ok {
			continue
		}
		utilization, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			continue
		}
		w := set.at(hw.kind, hw.scope)
		w.Duration = hw.duration
		w.Utilization = utilization
		if reset, ok := get(headerPrefix + hw.suffix + suffixReset); ok {
			w.ResetsAt = windowReset(reset, now, hw.duration)
		}
		if status, ok := get(headerPrefix + hw.suffix + suffixStatus); ok {
			w.Status = normalizeToken(status)
		}
	}

	markRepresentative(set, get, now)
	return set.slice()
}

// markRepresentative flags the window the provider currently treats as binding
// and fills that window's gaps from the unsuffixed triple, which describes it.
//
// No suffixed header family reports a per-family weekly cap, so a claim naming
// one has only the unsuffixed triple behind it. When that triple says rejected,
// the window is synthesized rather than dropped, because a refusal the response
// states outright must reach Window.Blocking. Its utilization is the 7-day
// reading, the closest bound the response carries on that family's spend. A
// claim naming any other unreported window marks nothing: the triple alone
// reports no utilization.
func markRepresentative(set *windowSet, get func(string) (string, bool), now time.Time) {
	claim, ok := get(headerClaim)
	if !ok {
		return
	}
	key, known := claimWindow(normalizeToken(claim))
	if !known {
		return
	}

	var status string
	if raw, ok := get(headerStatus); ok {
		status = normalizeToken(raw)
	}

	w, exists := set.lookup(key.kind, key.scope)
	if !exists {
		if key.kind != model.WindowWeeklyScoped || status != model.StatusRejected {
			return
		}
		w = set.at(key.kind, key.scope)
		w.Duration = model.WeeklyDuration
		if weekly, ok := set.lookup(model.WindowWeekly, ""); ok {
			w.Utilization = weekly.Utilization
		}
	}

	w.Active = true
	if w.Status == "" && status != "" {
		w.Status = status
	}
	if w.ResetsAt.IsZero() {
		if reset, ok := get(headerReset); ok {
			w.ResetsAt = windowReset(reset, now, w.Duration)
		}
	}
}
