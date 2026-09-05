package quota

import (
	"strconv"
	"testing"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

func TestParseUsagePayload(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		payload string
		want    []model.Window
	}{
		{
			name: "live payload early in the weekly window",
			file: "usage_early_week.json",
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 0.80,
					ResetsAt:    at(21, 0),
					Duration:    model.SessionDuration,
					Severity:    model.SeverityWarning,
					Active:      true,
				},
				{
					Kind:        model.WindowWeekly,
					Utilization: 0.04,
					ResetsAt:    day(10, 14),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityNormal,
				},
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "Fable",
					Utilization: 0,
					ResetsAt:    day(10, 14),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityNormal,
				},
			},
		},
		{
			name: "live payload with the session window spent",
			file: "usage_late_week.json",
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 1.0,
					ResetsAt:    at(18, 15),
					Duration:    model.SessionDuration,
					Severity:    model.SeverityCritical,
					Active:      true,
				},
				{
					Kind:        model.WindowWeekly,
					Utilization: 0.54,
					ResetsAt:    day(8, 9),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityWarning,
				},
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "Fable",
					Utilization: 0.67,
					ResetsAt:    day(8, 9),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityWarning,
				},
			},
		},
		{
			name: "unknown top-level keys and limit kinds are ignored",
			payload: `{
				"five_hour": {"utilization": 12.5, "resets_at": null},
				"seven_day": null,
				"pomegranate_drift": {"utilization": 99.0},
				"future_scalar": 7,
				"limits": [
					{"kind": "session", "percent": 12.5, "severity": "normal", "resets_at": null, "scope": null, "is_active": true},
					{"kind": "monthly_all", "percent": 90.0, "severity": "critical", "scope": null, "is_active": true},
					{"kind": "weekly_scoped", "percent": 33.0, "severity": "normal", "scope": {"model": {"id": "wombat-1", "display_name": "Wombat 1"}, "surface": "future_surface"}, "is_active": false}
				]
			}`,
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 0.125,
					Duration:    model.SessionDuration,
					Severity:    model.SeverityNormal,
					Active:      true,
				},
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "Wombat 1",
					Utilization: 0.33,
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityNormal,
				},
			},
		},
		{
			name: "scoped windows come from limits while the top-level keys are null",
			payload: `{
				"seven_day": {"utilization": 20.0, "resets_at": "2026-09-09T00:00:00.000000Z"},
				"seven_day_opus": null,
				"seven_day_sonnet": null,
				"limits": [
					{"kind": "weekly_all", "percent": 20.0, "severity": "normal", "resets_at": "2026-09-09T00:00:00.000000Z", "scope": null, "is_active": true},
					{"kind": "weekly_scoped", "percent": 71.5, "severity": "warning", "resets_at": "2026-09-09T00:00:00.000000Z", "scope": {"model": {"id": "claude-opus-4-6-20260514", "display_name": ""}, "surface": "claude_code"}, "is_active": false},
					{"kind": "weekly_scoped", "percent": 5.0, "severity": "normal", "resets_at": "2026-09-09T00:00:00.000000Z", "scope": {"model": {"id": "claude-sonnet-4-5", "display_name": "Claude Sonnet 4.5"}, "surface": "claude_code"}, "is_active": false}
				]
			}`,
			want: []model.Window{
				{
					Kind:        model.WindowWeekly,
					Utilization: 0.20,
					ResetsAt:    day(9, 0),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityNormal,
					Active:      true,
				},
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "Opus",
					Utilization: 0.715,
					ResetsAt:    day(9, 0),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityWarning,
				},
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "Sonnet",
					Utilization: 0.05,
					ResetsAt:    day(9, 0),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityNormal,
				},
			},
		},
		{
			name: "an unknown family with no display name is the sole reading",
			payload: `{
				"five_hour": null,
				"seven_day": null,
				"limits": [
					{"kind": "weekly_scoped", "percent": 12.0, "severity": "normal", "scope": {"model": {"id": "wombat-1", "display_name": ""}}, "is_active": true}
				]
			}`,
			want: []model.Window{
				{
					Kind:        model.WindowWeeklyScoped,
					Scope:       "wombat-1",
					Utilization: 0.12,
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityNormal,
					Active:      true,
				},
			},
		},
		{
			name: "a scoped limit with no model is skipped",
			payload: `{
				"five_hour": {"utilization": 3.0, "resets_at": null},
				"limits": [
					{"kind": "weekly_scoped", "percent": 42.0, "severity": "normal", "scope": null, "is_active": false}
				]
			}`,
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.03, Duration: model.SessionDuration},
			},
		},
		{
			name: "a null top-level window still reads from its limits entry",
			payload: `{
				"five_hour": {"utilization": null, "resets_at": null},
				"seven_day": null,
				"limits": [
					{"kind": "session", "percent": 61.0, "severity": "warning", "resets_at": "2026-09-04T20:00:00.000000Z", "scope": null, "is_active": true}
				]
			}`,
			want: []model.Window{
				{
					Kind:        model.WindowSession,
					Utilization: 0.61,
					ResetsAt:    at(20, 0),
					Duration:    model.SessionDuration,
					Severity:    model.SeverityWarning,
					Active:      true,
				},
			},
		},
		{
			name: "a limits entry with no percentage cannot create a window",
			payload: `{
				"seven_day": {"utilization": 8.0, "resets_at": null},
				"limits": [
					{"kind": "session", "percent": null, "severity": "normal", "scope": null, "is_active": true},
					{"kind": "weekly_all", "percent": null, "severity": "warning", "resets_at": "2026-09-07T00:00:00.000000Z", "scope": null, "is_active": true}
				]
			}`,
			want: []model.Window{
				{
					Kind:        model.WindowWeekly,
					Utilization: 0.08,
					ResetsAt:    day(7, 0),
					Duration:    model.WeeklyDuration,
					Severity:    model.SeverityWarning,
					Active:      true,
				},
			},
		},
		{
			name: "a reset outside the window it describes keeps the reading and drops the timeline",
			payload: `{
				"five_hour": {"utilization": 44.0, "resets_at": "2027-01-01T00:00:00.000000Z"},
				"seven_day": {"utilization": 9.0, "resets_at": "2026-08-01T00:00:00.000000Z"},
				"limits": []
			}`,
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.44, Duration: model.SessionDuration},
				{Kind: model.WindowWeekly, Utilization: 0.09, Duration: model.WeeklyDuration},
			},
		},
		{
			name: "an unparsable reset keeps the reading",
			payload: `{
				"five_hour": {"utilization": 50.0, "resets_at": "sometime next Tuesday"},
				"limits": []
			}`,
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.5, Duration: model.SessionDuration},
			},
		},
		{
			name: "a zero reading is a reading",
			payload: `{
				"five_hour": {"utilization": 0, "resets_at": "2026-09-04T19:00:00.000000Z"},
				"seven_day": {"utilization": 0.0, "resets_at": "2026-09-11T00:00:00.000000Z"},
				"limits": []
			}`,
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0, ResetsAt: at(19, 0), Duration: model.SessionDuration},
				{Kind: model.WindowWeekly, Utilization: 0, ResetsAt: day(11, 0), Duration: model.WeeklyDuration},
			},
		},
		{
			name: "a severity outside the known set is carried through",
			payload: `{
				"five_hour": {"utilization": 70.0, "resets_at": null},
				"limits": [
					{"kind": "session", "percent": 70.0, "severity": "Elevated", "scope": null, "is_active": false}
				]
			}`,
			want: []model.Window{
				{Kind: model.WindowSession, Utilization: 0.7, Duration: model.SessionDuration, Severity: "elevated"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.payload)
			if tc.file != "" {
				body = fixture(t, tc.file)
			}
			got, err := ParseUsagePayload(body, testNow)
			if err != nil {
				t.Fatalf("ParseUsagePayload: %v", err)
			}
			assertWindows(t, got, tc.want)
		})
	}
}

func TestParseUsagePayloadErrors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "empty body", payload: ""},
		{name: "not json", payload: "<html>gateway error</html>"},
		{name: "truncated json", payload: `{"five_hour": {"utilization": 4`},
		{name: "wrong type for utilization", payload: `{"five_hour": {"utilization": "80%"}}`},
		{name: "no windows at all", payload: `{}`},
		{name: "every window null", payload: `{"five_hour": null, "seven_day": null, "seven_day_opus": null, "limits": []}`},
		{name: "only unplaceable limits", payload: `{"limits": [{"kind": "quarterly_all", "percent": 5.0}]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseUsagePayload([]byte(tc.payload), testNow)
			if err == nil {
				t.Fatalf("ParseUsagePayload succeeded with %+v, want an error", got)
			}
			if Category(err) != CategoryBadJSON {
				t.Errorf("category = %q, want %q", Category(err), CategoryBadJSON)
			}
			if got != nil {
				t.Errorf("windows = %+v, want nil", got)
			}
		})
	}
}

// TestParseUsagePayloadScalesPercentages pins the conversion the rest of the
// plugin depends on: the endpoint speaks 0..100 and model.Window is 0..1.
func TestParseUsagePayloadScalesPercentages(t *testing.T) {
	for _, tc := range []struct {
		percent float64
		want    float64
	}{
		{percent: 0, want: 0},
		{percent: 4, want: 0.04},
		{percent: 67, want: 0.67},
		{percent: 80, want: 0.80},
		{percent: 100, want: 1.0},
		{percent: 137.5, want: 1.375},
	} {
		percent := strconv.FormatFloat(tc.percent, 'f', -1, 64)
		payload := `{"five_hour": {"utilization": ` + percent + `, "resets_at": null}, "limits": []}`
		got, err := ParseUsagePayload([]byte(payload), testNow)
		if err != nil {
			t.Fatalf("percent %v: %v", tc.percent, err)
		}
		assertWindows(t, got, []model.Window{
			{Kind: model.WindowSession, Utilization: tc.want, Duration: model.SessionDuration},
		})
	}
}

func TestScopeFamily(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		display string
		want    string
	}{
		{name: "fable display name", display: "Fable 5", want: "Fable"},
		{name: "opus display name", display: "Claude Opus 4.6", want: "Opus"},
		{name: "sonnet display name", display: "Claude Sonnet 4.5", want: "Sonnet"},
		{name: "haiku display name", display: "Claude Haiku 4.5", want: "Haiku"},
		{name: "family from id when display is empty", id: "claude-opus-4-6-20260514", want: "Opus"},
		{name: "unknown family keeps its display name", id: "wombat-1", display: "Wombat 1", want: "Wombat 1"},
		{name: "unknown family with no display name keeps its id", id: "wombat-1", want: "wombat-1"},
		{name: "no model at all", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var scope usageScope
			scope.Model.ID = tc.id
			scope.Model.DisplayName = tc.display
			if got := scopeFamily(&scope); got != tc.want {
				t.Errorf("scopeFamily = %q, want %q", got, tc.want)
			}
		})
	}
	if got := scopeFamily(nil); got != "" {
		t.Errorf("scopeFamily(nil) = %q, want empty", got)
	}
}
