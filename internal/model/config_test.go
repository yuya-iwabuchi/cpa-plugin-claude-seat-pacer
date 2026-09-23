package model

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestNormalizeFoldsProviderAndModelKeys(t *testing.T) {
	// The host names providers and models in lowercase, and every caller
	// matches against the folded form, so a config written in any other case
	// has to fold or it governs nothing.
	cfg := Config{
		Providers: []string{"Claude", "  ANTHROPIC  ", "", "   "},
		Models:    []string{"Claude-Opus-5", " claude-fable-5-1 ", ""},
	}
	cfg.Normalize()

	if want := []string{"claude", "anthropic"}; !equalStrings(cfg.Providers, want) {
		t.Errorf("Providers = %v, want %v", cfg.Providers, want)
	}
	if want := []string{"claude-opus-5", "claude-fable-5-1"}; !equalStrings(cfg.Models, want) {
		t.Errorf("Models = %v, want %v", cfg.Models, want)
	}
	for _, provider := range []string{"claude", "anthropic"} {
		if !cfg.GovernsProvider(provider) {
			t.Errorf("GovernsProvider(%q) = false", provider)
		}
	}
	if cfg.GovernsProvider("gemini") {
		t.Error("GovernsProvider(gemini) = true")
	}
	for _, modelID := range []string{"claude-opus-5", "Claude-Opus-5", " CLAUDE-FABLE-5-1 "} {
		if !cfg.GovernsModel(modelID) {
			t.Errorf("GovernsModel(%q) = false", modelID)
		}
	}
	if cfg.GovernsModel("claude-sonnet-4-5") {
		t.Error("GovernsModel returned true for a model outside the list")
	}
}

func TestNormalizeDefaultsAndClamps(t *testing.T) {
	var cfg Config
	cfg.Normalize()
	d := Defaults()

	if !equalStrings(cfg.Providers, d.Providers) {
		t.Errorf("Providers = %v, want the default %v", cfg.Providers, d.Providers)
	}
	if len(cfg.Models) != 0 {
		t.Errorf("Models = %v, want an empty list, which governs every model", cfg.Models)
	}
	if !cfg.GovernsModel("anything-at-all") {
		t.Error("an empty Models list did not govern every model")
	}
	if cfg.Affinity.TTL != d.Affinity.TTL || cfg.Affinity.MaxSessions != d.Affinity.MaxSessions {
		t.Errorf("affinity = %v %d, want the defaults", cfg.Affinity.TTL, cfg.Affinity.MaxSessions)
	}
	if cfg.Quota.PollInterval != d.Quota.PollInterval || cfg.Quota.UsageURL != d.Quota.UsageURL {
		t.Errorf("quota = %v %q, want the defaults", cfg.Quota.PollInterval, cfg.Quota.UsageURL)
	}
	if cfg.Web.HistoryLimit != d.Web.HistoryLimit {
		t.Errorf("history limit = %d, want %d", cfg.Web.HistoryLimit, d.Web.HistoryLimit)
	}

	// A poll interval under the floor spins the loop, a negative hysteresis
	// margin makes every challenger win, and a landing of nine is a
	// misconfiguration rather than a policy.
	cfg = Config{Quota: QuotaConfig{PollInterval: time.Second}, Pace: PaceConfig{HysteresisMargin: -1, LandingTarget: 9}}
	cfg.Normalize()
	if cfg.Quota.PollInterval != d.Quota.PollInterval {
		t.Errorf("poll interval = %v, want the default restored", cfg.Quota.PollInterval)
	}
	if cfg.Pace.HysteresisMargin != 0 {
		t.Errorf("hysteresis margin = %v, want 0", cfg.Pace.HysteresisMargin)
	}
	if cfg.Pace.LandingTarget != d.Pace.LandingTarget {
		t.Errorf("landing target = %v, want the default restored", cfg.Pace.LandingTarget)
	}
}

// A landing above full is the use-it-or-lose-it tilt, so it has to survive
// normalization; Target's clamp is what bounds it. Only a landing no curve
// could mean reads as a misconfiguration.
func TestNormalizeKeepsALandingAboveFull(t *testing.T) {
	d := Defaults()
	for _, tc := range []struct {
		name string
		in   float64
		want float64
	}{
		{"the default tilt", 1.10, 1.10},
		{"a steeper tilt", 2.5, 2.5},
		{"the top of the range", 4, 4},
		{"a deliberate safety margin", 0.9, 0.9},
		{"zero", 0, d.Pace.LandingTarget},
		{"negative", -1, d.Pace.LandingTarget},
		{"past the top of the range", 4.0001, d.Pace.LandingTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Pace: PaceConfig{LandingTarget: tc.in}}
			cfg.Normalize()
			if cfg.Pace.LandingTarget != tc.want {
				t.Errorf("LandingTarget %v normalized to %v, want %v", tc.in, cfg.Pace.LandingTarget, tc.want)
			}
		})
	}
}

// Shape is the one pace knob written as a name rather than a number, so a
// config that misspells it has to land on a curve the scorer can evaluate.
func TestNormalizeFoldsShapeAndSteepness(t *testing.T) {
	d := Defaults()
	for _, tc := range []struct {
		in   string
		want string
	}{
		{ShapeLinear, ShapeLinear},
		{ShapePower, ShapePower},
		{ShapeSigmoid, ShapeSigmoid},
		{"  SIGMOID  ", ShapeSigmoid},
		{"Power", ShapePower},
		{"", d.Pace.Shape},
		{"logarithmic", d.Pace.Shape},
	} {
		cfg := Config{Pace: PaceConfig{Shape: tc.in}}
		cfg.Normalize()
		if cfg.Pace.Shape != tc.want {
			t.Errorf("Shape %q normalized to %q, want %q", tc.in, cfg.Pace.Shape, tc.want)
		}
	}

	// A steepness of zero or below flattens the sigmoid into a line, which
	// silently ignores the shape the operator asked for.
	for _, bad := range []float64{0, -1, -8} {
		cfg := Config{Pace: PaceConfig{Steepness: bad}}
		cfg.Normalize()
		if cfg.Pace.Steepness != d.Pace.Steepness {
			t.Errorf("Steepness %v normalized to %v, want the default %v", bad, cfg.Pace.Steepness, d.Pace.Steepness)
		}
	}
	cfg := Config{Pace: PaceConfig{Steepness: 20}}
	cfg.Normalize()
	if cfg.Pace.Steepness != 20 {
		t.Errorf("Steepness = %v, want a positive setting kept", cfg.Pace.Steepness)
	}
}

func TestNormalizeRejectsNaNAndInf(t *testing.T) {
	// A NaN weight makes every score comparison false and an infinite one
	// makes a single window decide the pick, so neither may survive.
	d := Defaults()
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := Config{Pace: PaceConfig{
			CurveExponent:    bad,
			Steepness:        bad,
			LandingTarget:    bad,
			WeeklyWeight:     bad,
			SessionWeight:    bad,
			ScopedWeight:     bad,
			HysteresisMargin: bad,
		}}
		cfg.Normalize()
		if cfg.Pace != d.Pace {
			t.Errorf("pace after %v = %+v, want the defaults", bad, cfg.Pace)
		}
	}
}

func TestFamilyOf(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"claude-opus-5", FamilyOpus},
		{"Claude Sonnet 4.5", FamilySonnet},
		{"seven_day_opus", FamilyOpus},
		{"claude-fable-5-1", FamilyFable},
		{"CLAUDE-HAIKU-4-5", FamilyHaiku},
		{"gemini-3-pro", ""},
		{"", ""},
	} {
		if got := FamilyOf(tc.in); got != tc.want {
			t.Errorf("FamilyOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWindowBearsOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window Window
		family string
		want   bool
	}{
		{"session caps every model", Window{Kind: WindowSession}, FamilyOpus, true},
		{"weekly caps every model", Window{Kind: WindowWeekly}, "", true},
		{"scoped caps its own family", Window{Kind: WindowWeeklyScoped, Scope: FamilyOpus}, FamilyOpus, true},
		{"scoped named as the endpoint spells it", Window{Kind: WindowWeeklyScoped, Scope: "Claude Opus 4.5"}, FamilyOpus, true},
		{"scoped ignores another family", Window{Kind: WindowWeeklyScoped, Scope: FamilySonnet}, FamilyOpus, false},
		{"a model with no family has no scoped cap", Window{Kind: WindowWeeklyScoped, Scope: FamilyOpus}, "", false},
		{"an unknown kind caps nothing", Window{Kind: WindowKind("other")}, FamilyOpus, false},
	} {
		if got := tc.window.BearsOn(tc.family); got != tc.want {
			t.Errorf("%s: BearsOn(%q) = %v, want %v", tc.name, tc.family, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An exponent is only ever set to bend the curve, and only the power shape
// reads one. A config written before the shape key existed keeps the curve it
// asked for rather than going quietly linear on upgrade.
func TestAnExponentWithoutAShapeImpliesPower(t *testing.T) {
	d := Defaults()
	cases := []struct {
		name     string
		shape    string
		exponent float64
		want     string
	}{
		{"an exponent alone names the power shape", "", 1.35, ShapePower},
		{"the default exponent implies nothing", "", d.Pace.CurveExponent, d.Pace.Shape},
		{"an unset exponent implies nothing", "", 0, d.Pace.Shape},
		{"a named shape always wins", ShapeSigmoid, 1.35, ShapeSigmoid},
		{"a named linear shape is not overridden", ShapeLinear, 1.35, ShapeLinear},
		{"an unknown shape falls back rather than inferring", "spline", 1.35, d.Pace.Shape},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Pace: PaceConfig{Shape: tc.shape, CurveExponent: tc.exponent}}
			cfg.Normalize()
			if cfg.Pace.Shape != tc.want {
				t.Errorf("shape = %q, want %q", cfg.Pace.Shape, tc.want)
			}
		})
	}
}

// A negative weight would invert the pace preference and send work to the
// credential furthest over its target. Zero is meaningful and stays: it retires
// a window from the score without retiring the gates that read it.
func TestNormalizeCorrectsANegativeWeightAndKeepsZero(t *testing.T) {
	cfg := Config{Pace: PaceConfig{WeeklyWeight: -1, SessionWeight: -0.001, ScopedWeight: 0}}
	cfg.Normalize()

	if cfg.Pace.WeeklyWeight != 0 || cfg.Pace.SessionWeight != 0 {
		t.Errorf("weights = (%v, %v), want both corrected to 0",
			cfg.Pace.WeeklyWeight, cfg.Pace.SessionWeight)
	}
	if cfg.Pace.ScopedWeight != 0 {
		t.Errorf("scoped weight = %v, want the configured 0 kept", cfg.Pace.ScopedWeight)
	}

	// A positive weight is never touched.
	kept := Config{Pace: PaceConfig{WeeklyWeight: 2, SessionWeight: 0.25, ScopedWeight: 0.5}}
	kept.Normalize()
	if kept.Pace.WeeklyWeight != 2 || kept.Pace.SessionWeight != 0.25 || kept.Pace.ScopedWeight != 0.5 {
		t.Errorf("weights = (%v, %v, %v), want them kept as configured",
			kept.Pace.WeeklyWeight, kept.Pace.SessionWeight, kept.Pace.ScopedWeight)
	}
}

// The decision log allocates its whole ring at once, so a limit past the cap
// is an out-of-memory crash rather than a long history.
func TestNormalizeBoundsTheHistoryLimit(t *testing.T) {
	d := Defaults()
	for _, tc := range []struct {
		in, want int
	}{
		{1, 1},
		{10000, 10000},
		{10001, d.Web.HistoryLimit},
		{1000000000, d.Web.HistoryLimit},
		{0, d.Web.HistoryLimit},
		{-5, d.Web.HistoryLimit},
	} {
		cfg := Config{Web: WebConfig{HistoryLimit: tc.in}}
		cfg.Normalize()
		if cfg.Web.HistoryLimit != tc.want {
			t.Errorf("HistoryLimit %d normalized to %d, want %d", tc.in, cfg.Web.HistoryLimit, tc.want)
		}
	}
}

// Every credential's fetch in one poll shares a two-minute budget, so a
// timeout past a minute lets one hung fetch starve the rest.
func TestNormalizeBoundsTheRequestTimeout(t *testing.T) {
	d := Defaults()
	for _, tc := range []struct {
		in, want time.Duration
	}{
		{time.Second, time.Second},
		{time.Minute, time.Minute},
		{time.Minute + time.Nanosecond, d.Quota.RequestTimeout},
		{time.Hour, d.Quota.RequestTimeout},
		{0, d.Quota.RequestTimeout},
		{-time.Second, d.Quota.RequestTimeout},
	} {
		cfg := Config{Quota: QuotaConfig{RequestTimeout: tc.in}}
		cfg.Normalize()
		if cfg.Quota.RequestTimeout != tc.want {
			t.Errorf("RequestTimeout %v normalized to %v, want %v", tc.in, cfg.Quota.RequestTimeout, tc.want)
		}
	}
}

// A weight near the float ceiling turns a weighted slack into an infinite
// cost, and the status route cannot encode one.
func TestNormalizeBoundsTheWeights(t *testing.T) {
	d := Defaults()
	cfg := Config{Pace: PaceConfig{WeeklyWeight: 1e308, SessionWeight: 100.5, ScopedWeight: 100}}
	cfg.Normalize()
	if cfg.Pace.WeeklyWeight != d.Pace.WeeklyWeight {
		t.Errorf("weekly weight = %v, want the default %v", cfg.Pace.WeeklyWeight, d.Pace.WeeklyWeight)
	}
	if cfg.Pace.SessionWeight != d.Pace.SessionWeight {
		t.Errorf("session weight = %v, want the default %v", cfg.Pace.SessionWeight, d.Pace.SessionWeight)
	}
	if cfg.Pace.ScopedWeight != 100 {
		t.Errorf("scoped weight = %v, want the top of the range kept", cfg.Pace.ScopedWeight)
	}
}

// Every seat's bearer token goes to the usage URL, so plain http is allowed
// only where the request never leaves the machine.
func TestNormalizeAdmitsOnlyATrustedUsageURL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		kept bool
	}{
		{DefaultUsageURL, true},
		{"https://usage.internal.example/api/oauth/usage", true},
		{"HTTPS://usage.internal.example/api/oauth/usage", true},
		{"http://localhost:8080/usage", true},
		{"http://LOCALHOST/usage", true},
		{"http://127.0.0.1:41234/usage", true},
		{"http://127.8.9.10/usage", true},
		{"http://[::1]:41234/usage", true},
		{"http://usage.internal.example/api/oauth/usage", false},
		{"http://10.0.0.1/usage", false},
		{"http://localhost.example/usage", false},
		{"ftp://localhost/usage", false},
		{"file:///etc/passwd", false},
		{"/api/oauth/usage", false},
		{"https://", false},
		{"://bad", false},
		{"", false},
	} {
		cfg := Config{Quota: QuotaConfig{UsageURL: tc.in}}
		cfg.Normalize()
		want := DefaultUsageURL
		if tc.kept {
			want = tc.in
		}
		if cfg.Quota.UsageURL != want {
			t.Errorf("UsageURL %q normalized to %q, want %q", tc.in, cfg.Quota.UsageURL, want)
		}
	}
}

// A seat is read once per poll interval, so a reading that goes stale before
// the next one lands leaves an idle seat ineligible for part of every
// interval.
func TestNormalizeKeepsMaxStalenessPastTwoPolls(t *testing.T) {
	cfg := Config{Quota: QuotaConfig{PollInterval: 30 * time.Minute, MaxStaleness: 20 * time.Minute}}
	warnings := cfg.Normalize()
	if cfg.Quota.MaxStaleness != time.Hour {
		t.Errorf("MaxStaleness = %v, want it raised to twice the 30m poll interval", cfg.Quota.MaxStaleness)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "20m0s") || !strings.Contains(warnings[0], "30m0s") {
		t.Errorf("warnings = %q, want one naming the configured 20m0s and 30m0s", warnings)
	}

	// The default was never the operator's choice, so raising it is silent.
	unset := Config{Quota: QuotaConfig{PollInterval: 10 * time.Minute}}
	if warnings := unset.Normalize(); len(warnings) != 0 || unset.Quota.MaxStaleness != 20*time.Minute {
		t.Errorf("MaxStaleness = %v with warnings %q, want the default raised to 20m silently", unset.Quota.MaxStaleness, warnings)
	}

	kept := Config{Quota: QuotaConfig{PollInterval: 5 * time.Minute, MaxStaleness: 10 * time.Minute}}
	if warnings := kept.Normalize(); len(warnings) != 0 || kept.Quota.MaxStaleness != 10*time.Minute {
		t.Errorf("MaxStaleness = %v with warnings %q, want exactly two polls kept silently", kept.Quota.MaxStaleness, warnings)
	}
}
