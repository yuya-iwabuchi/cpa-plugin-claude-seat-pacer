package quota

import (
	"strconv"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
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

// scopeFable is the family name the 7d_oi window carries.
const scopeFable = "Fable"

// headerWindow binds one suffixed header family to a window identity. 7d_oi is
// seven_day_overage_included, which in 2026 is the Fable cap, and its suffix
// carries an underscore where the others carry none.
type headerWindow struct {
	suffix   string
	kind     model.WindowKind
	scope    string
	duration time.Duration
}

var headerWindows = []headerWindow{
	{suffix: "5h", kind: model.WindowSession, duration: model.SessionDuration},
	{suffix: "7d", kind: model.WindowWeekly, duration: model.WeeklyDuration},
	{suffix: "7d_oi", kind: model.WindowWeeklyScoped, scope: scopeFable, duration: model.WeeklyDuration},
}

// representativeClaims maps a Representative-Claim value onto the window it
// names. seven_day_overage_included is accepted alongside the four documented
// values because it is the 7d_oi window's own name.
var representativeClaims = map[string]windowKey{
	"five_hour":                  {kind: model.WindowSession},
	"seven_day":                  {kind: model.WindowWeekly},
	"seven_day_opus":             {kind: model.WindowWeeklyScoped, scope: "Opus"},
	"seven_day_sonnet":           {kind: model.WindowWeeklyScoped, scope: "Sonnet"},
	"seven_day_overage_included": {kind: model.WindowWeeklyScoped, scope: scopeFable},
}

// ParseResponseHeaders reads the unified rate-limit headers off a real
// response. Header lookup is case-insensitive.
//
// Header utilization is already a 0..1 fraction and is carried through
// unscaled, unlike the usage endpoint's 0..100 percentage. A value above 1 is
// legitimate and occurs once usage runs past a cap.
//
// Only windows that traffic has actually touched appear here, which is why a
// header read merges into a snapshot through Store.MergeHeaders instead of
// replacing it. A window whose utilization is absent or unparsable is left
// out, and no window is synthesized from the unsuffixed triple alone, which
// reports no utilization of its own.
func ParseResponseHeaders(h map[string][]string, now time.Time) []model.Window {
	get := headerGetter(h)
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
// A claim naming a window the response carried no headers for marks nothing.
func markRepresentative(set *windowSet, get func(string) (string, bool), now time.Time) {
	claim, ok := get(headerClaim)
	if !ok {
		return
	}
	key, known := representativeClaims[normalizeToken(claim)]
	if !known {
		return
	}
	w, exists := set.lookup(key.kind, key.scope)
	if !exists {
		return
	}
	w.Active = true
	if w.Status == "" {
		if status, ok := get(headerStatus); ok {
			w.Status = normalizeToken(status)
		}
	}
	if w.ResetsAt.IsZero() {
		if reset, ok := get(headerReset); ok {
			w.ResetsAt = windowReset(reset, now, w.Duration)
		}
	}
}

// headerGetter resolves header names case-insensitively, reporting each name's
// first non-empty value. An empty value reads as absent, so a header the
// provider sends blank never overwrites a reading.
func headerGetter(h map[string][]string) func(string) (string, bool) {
	byLower := make(map[string]string, len(h))
	for name, values := range h {
		key := strings.ToLower(name)
		if _, seen := byLower[key]; seen {
			continue
		}
		for _, v := range values {
			if strings.TrimSpace(v) != "" {
				byLower[key] = v
				break
			}
		}
	}
	return func(name string) (string, bool) {
		v, ok := byLower[strings.ToLower(name)]
		return v, ok
	}
}
