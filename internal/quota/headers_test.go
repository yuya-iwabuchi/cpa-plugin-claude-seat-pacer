package quota

import (
	"strconv"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func TestParseResponseHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string][]string
		want    []model.Window
	}{
		{
			name: "full header family with epoch resets",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Status":               {"allowed_warning"},
				"Anthropic-Ratelimit-Unified-Reset":                {epoch(21, 0)},
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
				"Anthropic-Ratelimit-Unified-5h-Status":            {"allowed_warning"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.8237"},
				"Anthropic-Ratelimit-Unified-5h-Reset":             {epoch(21, 0)},
				"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed"},
				"Anthropic-Ratelimit-Unified-7d-Utilization":       {"0.041"},
				"Anthropic-Ratelimit-Unified-7d-Reset":             {epochDay(10, 14)},
				"Anthropic-Ratelimit-Unified-7d_oi-Status":         {"allowed"},
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization":    {"0.67"},
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":          {epochDay(10, 14)},
			},
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 0.8237,
					ResetsAt:    at(21, 0),
					Duration:    model.SessionDuration,
					Status:      model.StatusAllowedWarning,
					Active:      true,
				},
				{
					Kind:        model.WindowWeekly,
					Utilization: 0.041,
					ResetsAt:    day(10, 14),
					Duration:    model.WeeklyDuration,
					Status:      model.StatusAllowed,
				},
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "Fable",
					Utilization: 0.67,
					ResetsAt:    day(10, 14),
					Duration:    model.WeeklyDuration,
					Status:      model.StatusAllowed,
				},
			},
		},
		{
			name: "utilization past the cap is carried through unscaled",
			headers: map[string][]string{
				"anthropic-ratelimit-unified-5h-utilization": {"1.0421"},
				"anthropic-ratelimit-unified-5h-status":      {"rejected"},
				"anthropic-ratelimit-unified-5h-reset":       {epoch(18, 15)},
			},
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 1.0421,
					ResetsAt:    at(18, 15),
					Duration:    model.SessionDuration,
					Status:      model.StatusRejected,
				},
			},
		},
		{
			name: "header names are matched case-insensitively",
			headers: map[string][]string{
				"ANTHROPIC-RATELIMIT-UNIFIED-5H-UTILIZATION":       {"0.5"},
				"anthropic-ratelimit-unified-5h-status":            {"ALLOWED"},
				"Anthropic-RateLimit-Unified-5h-Reset":             {epoch(20, 0)},
				"anthropic-ratelimit-unified-representative-claim": {"FIVE_HOUR"},
			},
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 0.5,
					ResetsAt:    at(20, 0),
					Duration:    model.SessionDuration,
					Status:      model.StatusAllowed,
					Active:      true,
				},
			},
		},
		{
			name: "rfc3339 reset",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.12"},
				"Anthropic-Ratelimit-Unified-7d-Reset":       {"2026-09-09T00:00:00Z"},
			},
			want: []model.Window{
				{Kind: model.WindowWeekly, Utilization: 0.12, ResetsAt: day(9, 0), Duration: model.WeeklyDuration},
			},
		},
		{
			name: "rfc3339 reset with fractional seconds",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.12"},
				"Anthropic-Ratelimit-Unified-7d-Reset":       {"2026-09-09T00:00:00.000000Z"},
			},
			want: []model.Window{
				{Kind: model.WindowWeekly, Utilization: 0.12, ResetsAt: day(9, 0), Duration: model.WeeklyDuration},
			},
		},
		{
			name: "http-date reset",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.33"},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {"Fri, 04 Sep 2026 20:30:00 GMT"},
			},
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.33, ResetsAt: at(20, 30), Duration: model.SessionDuration},
			},
		},
		{
			name: "the unsuffixed triple fills the representative window's gaps",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
				"Anthropic-Ratelimit-Unified-Reset":                {epoch(18, 15)},
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":       {"1.0"},
			},
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 1.0,
					ResetsAt:    at(18, 15),
					Duration:    model.SessionDuration,
					Status:      model.StatusRejected,
					Active:      true,
				},
			},
		},
		{
			name: "a window's own status wins over the unsuffixed one",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day"},
				"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed_warning"},
				"Anthropic-Ratelimit-Unified-7d-Utilization":       {"0.9"},
			},
			want: []model.Window{
				{
					Kind:        model.WindowWeekly,
					Utilization: 0.9,
					Duration:    model.WeeklyDuration,
					Status:      model.StatusAllowedWarning,
					Active:      true,
				},
			},
		},
		{
			name: "an allowed claim naming an unreported window synthesizes nothing",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Status":               {"allowed"},
				"Anthropic-Ratelimit-Unified-Reset":                {epochDay(10, 14)},
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_opus"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.2"},
			},
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.2, Duration: model.SessionDuration},
			},
		},
		{
			name: "a rejected claim for a family the headers do not report synthesizes its window",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
				"Anthropic-Ratelimit-Unified-Reset":                {epochDay(10, 14)},
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_opus"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.2"},
				"Anthropic-Ratelimit-Unified-7d-Utilization":       {"0.44"},
			},
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.2, Duration: model.SessionDuration},
				{Kind: model.WindowWeekly, Utilization: 0.44, Duration: model.WeeklyDuration},
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       model.FamilyOpus,
					Utilization: 0.44,
					ResetsAt:    day(10, 14),
					Duration:    model.WeeklyDuration,
					Status:      model.StatusRejected,
					Active:      true,
				},
			},
		},
		{
			name: "a rejected claim with no seven-day reading synthesizes a window at zero",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_sonnet"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.2"},
			},
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.2, Duration: model.SessionDuration},
				{
					Kind:     model.WindowWeeklyScoped,
					Scope:    model.FamilySonnet,
					Duration: model.WeeklyDuration,
					Status:   model.StatusRejected,
					Active:   true,
				},
			},
		},
		{
			name: "a family claim marks the window the headers already report",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_fable"},
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization":    {"0.67"},
			},
			want: []model.Window{
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       model.FamilyFable,
					Utilization: 0.67,
					Duration:    model.WeeklyDuration,
					Active:      true,
				},
			},
		},
		{
			name: "the overage-included claim names the fable window",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization":    {"0.67"},
				"Anthropic-Ratelimit-Unified-7d_oi-Reset":          {epochDay(8, 9)},
			},
			want: []model.Window{
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "Fable",
					Utilization: 0.67,
					ResetsAt:    day(8, 9),
					Duration:    model.WeeklyDuration,
					Active:      true,
				},
			},
		},
		{
			name: "an unparsable utilization drops the window",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {"n/a"},
				"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.3"},
			},
			want: []model.Window{
				{Kind: model.WindowWeekly, Utilization: 0.3, Duration: model.WeeklyDuration},
			},
		},
		{
			name: "a blank header value reads as absent",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
				"Anthropic-Ratelimit-Unified-5h-Status":      {"  "},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {""},
			},
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.42, Duration: model.SessionDuration},
			},
		},
		{
			name: "a reset outside the window keeps the reading",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {epochDay(30, 0)},
			},
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.42, Duration: model.SessionDuration},
			},
		},
		{
			name: "an unknown claim marks nothing",
			headers: map[string][]string{
				"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_wombat"},
				"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.2"},
			},
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.2, Duration: model.SessionDuration},
			},
		},
		{
			name:    "no rate-limit headers at all",
			headers: map[string][]string{"Content-Type": {"application/json"}},
			want:    nil,
		},
		{
			name:    "nil header map",
			headers: nil,
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseResponseHeaders(tc.headers, testNow)
			assertWindows(t, got, tc.want)
		})
	}
}

// TestParseResponseHeadersKeepARejectionBlocking pins the reason the scoped
// window is synthesized at all: routing must see the refusal.
func TestParseResponseHeadersKeepARejectionBlocking(t *testing.T) {
	for _, claim := range []string{"seven_day_opus", "seven_day_sonnet", "seven_day_haiku"} {
		got := ParseResponseHeaders(map[string][]string{
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {claim},
			"Anthropic-Ratelimit-Unified-7d-Utilization":       {"0.9"},
		}, testNow)

		blocking := false
		for _, w := range got {
			if w.Kind == model.WindowWeeklyScoped && w.Blocking() && w.Active {
				blocking = true
			}
		}
		if !blocking {
			t.Errorf("claim %q produced %+v, want a blocking active scoped window", claim, got)
		}
	}
}

// TestParseResponseHeadersDoesNotScale is the counterpart to
// TestParseUsagePayloadScalesPercentages: headers already report a fraction,
// so dividing here would understate usage a hundredfold.
func TestParseResponseHeadersDoesNotScale(t *testing.T) {
	for _, raw := range []string{"0", "0.04", "0.67", "0.8", "1", "1.375"} {
		want, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		got := ParseResponseHeaders(map[string][]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {raw},
		}, testNow)
		assertWindows(t, got, []model.Window{
			{Kind: model.WindowSession, Utilization: want, Duration: model.SessionDuration},
		})
	}
}

// TestParseResponseHeadersRejectsNonFiniteUtilization covers a reading
// strconv.ParseFloat accepts with a nil error: a non-finite utilization would
// otherwise reach the scorer and fail every status encode.
func TestParseResponseHeadersFloorsANegativeReading(t *testing.T) {
	got := ParseResponseHeaders(map[string][]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"-0.4"},
	}, at(20, 0))
	if len(got) != 1 || got[0].Utilization != 0 {
		t.Fatalf("windows = %+v, want one session window at 0", got)
	}
}

func TestParseResponseHeadersRejectsNonFiniteUtilization(t *testing.T) {
	for _, raw := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity", "-INFINITY"} {
		t.Run(raw, func(t *testing.T) {
			got := ParseResponseHeaders(map[string][]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {raw},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {epoch(21, 0)},
				"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.25"},
			}, at(20, 0))
			for _, w := range got {
				if w.Kind == model.WindowSession {
					t.Fatalf("session window kept a %q utilization: %v", raw, w.Utilization)
				}
			}
			if len(got) != 1 || got[0].Utilization != 0.25 {
				t.Fatalf("windows = %+v, want only the 7d reading", got)
			}
		})
	}
}
