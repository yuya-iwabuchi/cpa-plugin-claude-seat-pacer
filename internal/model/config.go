package model

import (
	"math"
	"time"
)

// Config is the plugin's block under plugins.configs.<plugin-id> in the
// CLIProxyAPI config file. The host owns Enabled and Priority and injects them
// even when absent; every other key belongs to the plugin.
type Config struct {
	Enabled  bool `yaml:"enabled" json:"enabled"`
	Priority int  `yaml:"priority" json:"priority"`

	// Providers limits which upstream providers the plugin governs. A request
	// for any other provider is declined so the host's own selector runs.
	Providers []string `yaml:"providers" json:"providers"`
	// Models limits which models the plugin governs. Empty governs all.
	Models []string `yaml:"models" json:"models"`

	Affinity AffinityConfig `yaml:"affinity" json:"affinity"`
	Pace     PaceConfig     `yaml:"pace" json:"pace"`
	Quota    QuotaConfig    `yaml:"quota" json:"quota"`
	Web      WebConfig      `yaml:"web" json:"web"`
}

// AffinityConfig governs conversation stickiness.
type AffinityConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// TTL bounds idle time rather than total session length: reuse refreshes
	// it.
	TTL time.Duration `yaml:"ttl" json:"ttl"`
	// MaxSessions caps the binding table, evicting least-recently-seen.
	MaxSessions int `yaml:"max-sessions" json:"max_sessions"`
	// Subagents pins a subagent request to its parent's credential. Subagents
	// replay the parent's prefix, so sharing its credential is a large cache
	// win at the cost of parallel spread.
	Subagents bool `yaml:"subagents" json:"subagents"`
	// OverrideThreshold serves an established binding even when the bound
	// credential has fallen behind the pace curve. One extra request on a
	// nearly-spent credential costs far less than rebuilding a cached prefix
	// on another one.
	OverrideThreshold bool `yaml:"override-threshold" json:"override_threshold"`
}

// PaceConfig governs cold-start credential choice.
//
// The pace model compares each window's observed utilization against the
// utilization a target curve expects at the same point in the window. The
// credential furthest behind its curve wins, which drains a window that is
// about to reset while holding back one with days left.
type PaceConfig struct {
	// CurveExponent is gamma in target = LandingTarget * elapsed^gamma.
	// 1.0 is linear and means "spend evenly", which puts the target at 50%
	// halfway through a window and 90% at nine tenths elapsed. Above 1.0 is
	// conservative early and aggressive late; below 1.0 front-loads spending
	// and risks early exhaustion.
	CurveExponent float64 `yaml:"curve-exponent" json:"curve_exponent"`
	// LandingTarget is the utilization the curve aims to reach at window end.
	// Below 1.0 leaves a deliberate safety margin.
	LandingTarget float64 `yaml:"landing-target" json:"landing_target"`

	// WeeklyWeight scales the weekly window's slack. The weekly window is the
	// resource actually lost at reset, so it dominates.
	WeeklyWeight float64 `yaml:"weekly-weight" json:"weekly_weight"`
	// SessionWeight scales the 5-hour window's slack. Non-zero keeps traffic
	// from slamming one credential's session window when weekly slack ties.
	SessionWeight float64 `yaml:"session-weight" json:"session_weight"`
	// ScopedWeight scales a model-family weekly window's slack when the
	// requested model falls in that family.
	ScopedWeight float64 `yaml:"scoped-weight" json:"scoped_weight"`

	// RawWeight penalizes absolute utilization independently of pace. Pace
	// slack alone under-penalizes a credential at 80% that is merely on
	// schedule, which starves idle siblings; this term restores spread.
	RawWeight float64 `yaml:"raw-utilization-weight" json:"raw_utilization_weight"`

	// HysteresisMargin is the score gap a challenger must beat before an
	// established binding moves. Zero makes routing flap at window edges.
	HysteresisMargin float64 `yaml:"hysteresis-margin" json:"hysteresis_margin"`
	// HardCutoff is the utilization at or above which a credential is
	// ineligible for a cold pick.
	HardCutoff float64 `yaml:"hard-cutoff" json:"hard_cutoff"`
}

// QuotaConfig governs quota observation.
type QuotaConfig struct {
	// PollInterval is how often each credential's usage endpoint is read.
	// Claude Code itself caches this for an hour, so minutes are generous.
	PollInterval time.Duration `yaml:"poll-interval" json:"poll_interval"`
	// RequestTimeout bounds one usage fetch.
	RequestTimeout time.Duration `yaml:"request-timeout" json:"request_timeout"`
	// MaxStaleness is the age past which a snapshot stops being trusted and
	// the plugin declines rather than routing on stale data.
	MaxStaleness time.Duration `yaml:"max-staleness" json:"max_staleness"`
	// UsageURL is the endpoint read for per-window utilization.
	UsageURL string `yaml:"usage-url" json:"usage_url"`
}

// WebConfig governs the built-in status app.
type WebConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// HistoryLimit caps retained routing decisions.
	HistoryLimit int `yaml:"history-limit" json:"history_limit"`
}

// DefaultUsageURL reports per-window utilization for a subscription credential
// without consuming message quota.
const DefaultUsageURL = "https://api.anthropic.com/api/oauth/usage"

// Defaults returns a config that is safe to run unattended: stickiness on,
// linear pace curve, and a hard cutoff just short of exhaustion.
func Defaults() Config {
	return Config{
		Providers: []string{"claude"},
		Affinity: AffinityConfig{
			Enabled:           true,
			TTL:               time.Hour,
			MaxSessions:       65536,
			Subagents:         true,
			OverrideThreshold: true,
		},
		Pace: PaceConfig{
			CurveExponent:    1.0,
			LandingTarget:    1.0,
			WeeklyWeight:     1.0,
			SessionWeight:    0.35,
			ScopedWeight:     0.5,
			RawWeight:        0.25,
			HysteresisMargin: 0.05,
			HardCutoff:       0.98,
		},
		Quota: QuotaConfig{
			PollInterval:   2 * time.Minute,
			RequestTimeout: 10 * time.Second,
			MaxStaleness:   15 * time.Minute,
			UsageURL:       DefaultUsageURL,
		},
		Web: WebConfig{
			Enabled:      true,
			HistoryLimit: 500,
		},
	}
}

// Normalize fills zero values with defaults and clamps out-of-range settings
// so a partial or hostile config block cannot produce a scorer that divides by
// zero, compares against NaN, or a poller that spins.
func (c *Config) Normalize() {
	d := Defaults()
	for _, f := range []struct {
		v   *float64
		def float64
	}{
		{&c.Pace.CurveExponent, d.Pace.CurveExponent},
		{&c.Pace.LandingTarget, d.Pace.LandingTarget},
		{&c.Pace.WeeklyWeight, d.Pace.WeeklyWeight},
		{&c.Pace.SessionWeight, d.Pace.SessionWeight},
		{&c.Pace.ScopedWeight, d.Pace.ScopedWeight},
		{&c.Pace.RawWeight, d.Pace.RawWeight},
		{&c.Pace.HysteresisMargin, d.Pace.HysteresisMargin},
		{&c.Pace.HardCutoff, d.Pace.HardCutoff},
	} {
		if math.IsNaN(*f.v) || math.IsInf(*f.v, 0) {
			*f.v = f.def
		}
	}
	if len(c.Providers) == 0 {
		c.Providers = d.Providers
	}
	if c.Affinity.TTL <= 0 {
		c.Affinity.TTL = d.Affinity.TTL
	}
	if c.Affinity.MaxSessions <= 0 {
		c.Affinity.MaxSessions = d.Affinity.MaxSessions
	}
	if c.Pace.CurveExponent <= 0 {
		c.Pace.CurveExponent = d.Pace.CurveExponent
	}
	if c.Pace.LandingTarget <= 0 {
		c.Pace.LandingTarget = d.Pace.LandingTarget
	}
	if c.Pace.HardCutoff <= 0 || c.Pace.HardCutoff > 2 {
		c.Pace.HardCutoff = d.Pace.HardCutoff
	}
	if c.Pace.HysteresisMargin < 0 {
		c.Pace.HysteresisMargin = 0
	}
	if c.Quota.PollInterval < 30*time.Second {
		c.Quota.PollInterval = d.Quota.PollInterval
	}
	if c.Quota.RequestTimeout <= 0 {
		c.Quota.RequestTimeout = d.Quota.RequestTimeout
	}
	if c.Quota.MaxStaleness <= 0 {
		c.Quota.MaxStaleness = d.Quota.MaxStaleness
	}
	if c.Quota.UsageURL == "" {
		c.Quota.UsageURL = d.Quota.UsageURL
	}
	if c.Web.HistoryLimit <= 0 {
		c.Web.HistoryLimit = d.Web.HistoryLimit
	}
}

// GovernsProvider reports whether the plugin should decide for a provider.
func (c Config) GovernsProvider(provider string) bool {
	for _, p := range c.Providers {
		if p == provider {
			return true
		}
	}
	return false
}

// GovernsModel reports whether the plugin should decide for a model. An empty
// Models list governs every model.
func (c Config) GovernsModel(model string) bool {
	if len(c.Models) == 0 {
		return true
	}
	for _, m := range c.Models {
		if m == model {
			return true
		}
	}
	return false
}
