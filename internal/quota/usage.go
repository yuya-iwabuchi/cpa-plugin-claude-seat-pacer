package quota

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
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

// usageWindow is one top-level window object. Utilization is a percentage in
// 0..100 and is a pointer because 0 is a real reading and null is not: the
// model-family windows (seven_day_opus, seven_day_sonnet and
// friends) are null on current plans.
type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// usageLimit is one limits[] entry. Percent is a percentage in 0..100 on the
// same terms as usageWindow.Utilization.
type usageLimit struct {
	Kind     string      `json:"kind"`
	Group    string      `json:"group"`
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
	Surface string `json:"surface"`
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

// modelFamilies are the scope names a model-family window carries. Anthropic
// publishes no family field, so the family is read off the scoped model's
// display name and, failing that, its id.
var modelFamilies = []string{"Opus", "Sonnet", "Haiku", "Fable"}

// ParseUsagePayload reads an /api/oauth/usage body into windows.
//
// The endpoint reports utilization as a percentage in 0..100 while
// model.Window carries a 0..1 fraction, so each reading is divided by 100
// exactly once here.
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
		w.Utilization = *p.FiveHour.Utilization / percentScale
		w.ResetsAt = windowReset(p.FiveHour.ResetsAt, now, w.Duration)
	}
	if p.SevenDay != nil && p.SevenDay.Utilization != nil {
		w := set.at(model.WindowWeekly, "")
		w.Duration = model.WeeklyDuration
		w.Utilization = *p.SevenDay.Utilization / percentScale
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
			w.Utilization = *l.Percent / percentScale
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
// empty string for a limit with no model, which cannot be attributed to a
// family. An unrecognized model keeps its display name, so a family Anthropic
// adds shows up in the status UI instead of vanishing.
func scopeFamily(s *usageScope) string {
	if s == nil {
		return ""
	}
	for _, candidate := range []string{s.Model.DisplayName, s.Model.ID} {
		lower := strings.ToLower(candidate)
		for _, family := range modelFamilies {
			if strings.Contains(lower, strings.ToLower(family)) {
				return family
			}
		}
	}
	return strings.TrimSpace(s.Model.DisplayName)
}

// normalizeToken folds a provider enum value to the lower-case form the
// model.Status* and model.Severity* constants use.
func normalizeToken(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
