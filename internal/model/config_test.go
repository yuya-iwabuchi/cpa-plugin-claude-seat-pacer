package model

import (
	"math"
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

	// A poll interval under the floor spins the loop, and a negative
	// hysteresis margin makes every challenger win.
	cfg = Config{Quota: QuotaConfig{PollInterval: time.Second}, Pace: PaceConfig{HysteresisMargin: -1, HardCutoff: 9}}
	cfg.Normalize()
	if cfg.Quota.PollInterval != d.Quota.PollInterval {
		t.Errorf("poll interval = %v, want the default restored", cfg.Quota.PollInterval)
	}
	if cfg.Pace.HysteresisMargin != 0 {
		t.Errorf("hysteresis margin = %v, want 0", cfg.Pace.HysteresisMargin)
	}
	if cfg.Pace.HardCutoff != d.Pace.HardCutoff {
		t.Errorf("hard cutoff = %v, want the default restored", cfg.Pace.HardCutoff)
	}
}

func TestNormalizeRejectsNaNAndInf(t *testing.T) {
	// A NaN weight makes every score comparison false and an infinite one
	// makes a single window decide the pick, so neither may survive.
	d := Defaults()
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := Config{Pace: PaceConfig{
			CurveExponent:    bad,
			LandingTarget:    bad,
			WeeklyWeight:     bad,
			SessionWeight:    bad,
			ScopedWeight:     bad,
			RawWeight:        bad,
			HysteresisMargin: bad,
			HardCutoff:       bad,
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
