package quota

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// usagePayload is the subset of /api/oauth/usage this package reads. Ignoring
// unknown keys is load-bearing: the endpoint also carries a rotating set of
// codenamed fields, null or zero on current plans, plus spend and subscription
// metadata that routing has no use for.
type usagePayload struct {
	FiveHour *usageWindow `json:"five_hour"`
	SevenDay *usageWindow `json:"seven_day"`
	Limits   []usageLimit `json:"limits"`
}

// usageWindow is one top-level window object. Utilization is a pointer because
// 0 is a real reading and null is not.
type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// usageLimit is one limits[] entry. Percent is a pointer on the same terms as
// usageWindow.Utilization.
type usageLimit struct {
	Kind     string      `json:"kind"`
	Percent  *float64    `json:"percent"`
	Severity string      `json:"severity"`
	ResetsAt string      `json:"resets_at"`
	Scope    *usageScope `json:"scope"`
	IsActive bool        `json:"is_active"`
}

// usageScope names the model a scoped limit covers.
type usageScope struct {
	Model struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
}

// limits[] kinds. An entry of any other kind is ignored so the endpoint can
// grow new ones without breaking a fetch.
const (
	limitSession      = "session"
	limitWeeklyAll    = "weekly_all"
	limitWeeklyScoped = "weekly_scoped"
)

// percentScale converts the endpoint's 0..100 percentage to the 0..1 fraction
// model.Window carries. The response headers already report a fraction, so
// this division belongs to the endpoint path alone.
const percentScale = 100

// nonNegative floors a parsed utilization at zero. No window is spent below
// empty, and a negative reading would rank its seat further behind plan than
// any real one.
func nonNegative(utilization float64) float64 {
	return max(utilization, 0)
}

// ParseUsagePayload reads an /api/oauth/usage body into windows.
//
// Model-family windows come from limits[] entries of kind weekly_scoped rather
// than the top-level seven_day_<family> keys, because those keys are null on
// current plans while the limits[] entries are populated. Unknown kinds,
// unknown scopes and unknown top-level keys are ignored, and a window with no
// utilization reading is left out entirely. A body that parses into no window
// is an error: it means the shape moved and routing must not treat a
// credential as idle on that basis.
func ParseUsagePayload(b []byte, now time.Time) ([]model.Window, error) {
	var p usagePayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, &FetchError{Category: CategoryBadJSON, Detail: "usage payload is not valid JSON"}
	}

	set := newWindowSet()
	if p.FiveHour != nil && p.FiveHour.Utilization != nil {
		w := set.at(model.WindowSession, "")
		w.Duration = model.SessionDuration
		w.Utilization = nonNegative(*p.FiveHour.Utilization / percentScale)
		w.ResetsAt = windowReset(p.FiveHour.ResetsAt, now, w.Duration)
	}
	if p.SevenDay != nil && p.SevenDay.Utilization != nil {
		w := set.at(model.WindowWeekly, "")
		w.Duration = model.WeeklyDuration
		w.Utilization = nonNegative(*p.SevenDay.Utilization / percentScale)
		w.ResetsAt = windowReset(p.SevenDay.ResetsAt, now, w.Duration)
	}

	for _, l := range p.Limits {
		kind, scope, duration, ok := limitTarget(l)
		if !ok {
			continue
		}
		w, exists := set.lookup(kind, scope)
		if !exists {
			// A limits[] entry without a percentage carries no reading of its
			// own, so it can enrich a window but never create one.
			if l.Percent == nil {
				continue
			}
			w = set.at(kind, scope)
			w.Duration = duration
		}
		if l.Percent != nil {
			w.Utilization = nonNegative(*l.Percent / percentScale)
		}
		if w.ResetsAt.IsZero() {
			w.ResetsAt = windowReset(l.ResetsAt, now, w.Duration)
		}
		if severity := normalizeToken(l.Severity); severity != "" {
			w.Severity = severity
		}
		if l.IsActive {
			w.Active = true
		}
	}

	windows := set.slice()
	if len(windows) == 0 {
		return nil, &FetchError{Category: CategoryBadJSON, Detail: "usage payload carries no readable window"}
	}
	return windows, nil
}

// limitTarget maps a limits[] entry onto a window identity and duration, and
// reports false for an entry routing cannot place.
func limitTarget(l usageLimit) (model.WindowKind, string, time.Duration, bool) {
	switch normalizeToken(l.Kind) {
	case limitSession:
		return model.WindowSession, "", model.SessionDuration, true
	case limitWeeklyAll:
		return model.WindowWeekly, "", model.WeeklyDuration, true
	case limitWeeklyScoped:
		scope := scopeFamily(l.Scope)
		if scope == "" {
			return "", "", 0, false
		}
		return model.WindowWeeklyScoped, scope, model.WeeklyDuration, true
	}
	return "", "", 0, false
}

// scopeFamily names the model family a scoped limit covers, and reports the
// empty string only for a limit with no model at all, which cannot be
// attributed to a family. A model naming no known family keeps its own display
// name, or its id when the endpoint sends no display name, so a family
// Anthropic adds shows up in the status UI instead of vanishing.
func scopeFamily(s *usageScope) string {
	if s == nil {
		return ""
	}
	for _, candidate := range []string{s.Model.DisplayName, s.Model.ID} {
		if family := model.FamilyOf(candidate); family != "" {
			return family
		}
	}
	for _, candidate := range []string{s.Model.DisplayName, s.Model.ID} {
		if name := strings.TrimSpace(candidate); name != "" {
			return name
		}
	}
	return ""
}

// normalizeToken folds a provider enum value to the lower-case form the
// model.Status* and model.Severity* constants use.
func normalizeToken(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
