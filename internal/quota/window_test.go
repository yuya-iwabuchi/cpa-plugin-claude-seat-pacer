package quota

import (
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func TestParseInstant(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "epoch seconds", raw: epoch(21, 0), want: "2026-09-04T21:00:00Z", ok: true},
		{name: "epoch milliseconds", raw: epochMillis(21, 0), want: "2026-09-04T21:00:00Z", ok: true},
		{name: "rfc3339", raw: "2026-09-04T21:00:00Z", want: "2026-09-04T21:00:00Z", ok: true},
		{name: "rfc3339 with fraction", raw: "2026-09-04T21:00:00.123456Z", want: "2026-09-04T21:00:00.123456Z", ok: true},
		{name: "rfc3339 with offset", raw: "2026-09-04T17:00:00-04:00", want: "2026-09-04T21:00:00Z", ok: true},
		{name: "http-date", raw: "Fri, 04 Sep 2026 21:00:00 GMT", want: "2026-09-04T21:00:00Z", ok: true},
		{name: "rfc1123 with numeric zone", raw: "Fri, 04 Sep 2026 21:00:00 +0000", want: "2026-09-04T21:00:00Z", ok: true},
		{name: "padded", raw: "  " + epoch(21, 0) + "  ", want: "2026-09-04T21:00:00Z", ok: true},
		{name: "empty", raw: "", ok: false},
		{name: "blank", raw: "   ", ok: false},
		{name: "prose", raw: "next tuesday", ok: false},
		{name: "decimal", raw: "1788555600.5", ok: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseInstant(tc.raw)
			if ok != tc.ok {
				t.Fatalf("parseInstant(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if formatted := got.Format(time.RFC3339Nano); formatted != tc.want {
				t.Errorf("parseInstant(%q) = %s, want %s", tc.raw, formatted, tc.want)
			}
		})
	}
}

func TestWindowReset(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		duration time.Duration
		want     time.Time
	}{
		{
			name:     "inside the session window",
			raw:      epoch(20, 0),
			duration: model.SessionDuration,
			want:     at(20, 0),
		},
		{
			name:     "at the far edge of the session window",
			raw:      epoch(22, 30),
			duration: model.SessionDuration,
			want:     at(22, 30),
		},
		{
			name:     "past the session window but inside the weekly one",
			raw:      epochDay(9, 0),
			duration: model.SessionDuration,
			want:     time.Time{},
		},
		{
			name:     "inside the weekly window",
			raw:      epochDay(9, 0),
			duration: model.WeeklyDuration,
			want:     day(9, 0),
		},
		{
			name:     "already elapsed",
			raw:      epoch(16, 0),
			duration: model.SessionDuration,
			want:     time.Time{},
		},
		{
			name:     "within the skew tolerance of now",
			raw:      epoch(17, 29),
			duration: model.SessionDuration,
			want:     at(17, 29),
		},
		{
			name:     "unparsable",
			raw:      "soon",
			duration: model.SessionDuration,
			want:     time.Time{},
		},
		{
			name:     "millisecond epoch inside the session window",
			raw:      epochMillis(20, 0),
			duration: model.SessionDuration,
			want:     at(20, 0),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := windowReset(tc.raw, testNow, tc.duration)
			if !got.Equal(tc.want) {
				t.Errorf("windowReset = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWindowSetPreservesInsertionOrder(t *testing.T) {
	set := newWindowSet()
	set.at(model.WindowWeekly, "").Utilization = 0.1
	set.at(model.WindowSession, "").Utilization = 0.2
	set.at(model.WindowWeeklyScoped, "Fable").Utilization = 0.3
	// Revisiting an identity enriches the existing reading rather than
	// appending a second one.
	set.at(model.WindowWeekly, "").Severity = model.SeverityWarning

	got := set.slice()
	want := []model.Window{
		{Kind: model.WindowWeekly, Utilization: 0.1, Severity: model.SeverityWarning},
		{Kind: model.WindowSession, Utilization: 0.2},
		{Kind: model.WindowWeeklyScoped, Scope: "Fable", Utilization: 0.3},
	}
	assertWindows(t, got, want)

	if got := newWindowSet().slice(); got != nil {
		t.Errorf("empty set slice = %+v, want nil", got)
	}
}
