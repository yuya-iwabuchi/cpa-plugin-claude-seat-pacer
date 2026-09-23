package model

import (
	"math"
	"net"
	"net/url"
	"strings"
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
	// Shape names the curve the target follows across a window: "linear"
	// spends evenly, "power" bends it by CurveExponent, "sigmoid" holds back early,
	// climbs through the middle and eases off near the end. An unknown name
	// reads as linear.
	Shape string `yaml:"shape" json:"shape"`
	// CurveExponent is gamma in landing * elapsed^gamma, and applies to the
	// power shape alone. Above 1.0 is conservative early and aggressive late;
	// below 1.0 front-loads spending and risks early exhaustion.
	CurveExponent float64 `yaml:"curve-exponent" json:"curve_exponent"`
	// Steepness is the sigmoid's slope through its midpoint, and applies to
	// that shape alone. Higher is more S-shaped; near zero is almost linear.
	Steepness float64 `yaml:"steepness" json:"steepness"`
	// LandingTarget is the utilization the curve aims for at window end,
	// before Target clamps it to full.
	//
	// Above 1.0 tilts the pick toward a credential near its reset: weekly
	// quota is perishable, so budget that expires in hours is worth spending
	// ahead of budget that has days left. The clamp bounds that tilt rather
	// than making it safe — a full window is gated in ScoreAuth, because a
	// clamped slack of zero still beats a credential running over its own
	// curve. The clamp does fix where the tilt saturates: the target reaches
	// full where Shape reaches the reciprocal of the landing, which is that
	// fraction of the way through a linear window and earlier or later under
	// the other shapes. Below 1.0 leaves a deliberate safety margin instead.
	LandingTarget float64 `yaml:"landing-target" json:"landing_target"`

	// WeeklyWeight scales the weekly window's slack. The weekly window is the
	// resource actually lost at reset, so it dominates.
	WeeklyWeight float64 `yaml:"weekly-weight" json:"weekly_weight"`
	// SessionWeight scales the 5-hour window's slack. It is zero: the session
	// window is a rate limit rather than a budget, so there is nothing to pace
	// against — unused session capacity is not carried, and the window resets
	// several times a day. A refusal recorded against it, a reading of it that
	// is not finite, or a reading of it at full still gates the credential —
	// each is an observation rather than a forecast.
	// Non-zero restores a short-horizon term when weekly slack ties.
	SessionWeight float64 `yaml:"session-weight" json:"session_weight"`
	// ScopedWeight scales a model-family weekly window's slack when the
	// requested model falls in that family. Both weekly windows are paced
	// against the same curve and differ only in weight.
	ScopedWeight float64 `yaml:"scoped-weight" json:"scoped_weight"`

	// HysteresisMargin is the score gap a challenger must beat before an
	// established binding moves. Zero makes routing flap at window edges.
	HysteresisMargin float64 `yaml:"hysteresis-margin" json:"hysteresis_margin"`
}

// Curve shapes for PaceConfig.Shape.
const (
	ShapeLinear  = "linear"
	ShapePower   = "power"
	ShapeSigmoid = "sigmoid"
)

// QuotaConfig governs quota observation.
type QuotaConfig struct {
	// PollInterval is how often each credential's usage endpoint is read.
	// Claude Code itself caches this for an hour, so minutes are generous.
	PollInterval time.Duration `yaml:"poll-interval" json:"poll_interval"`
	// RequestTimeout bounds one usage fetch, at most a minute.
	RequestTimeout time.Duration `yaml:"request-timeout" json:"request_timeout"`
	// MaxStaleness is the age past which a snapshot stops being trusted and
	// the plugin declines rather than routing on stale data.
	MaxStaleness time.Duration `yaml:"max-staleness" json:"max_staleness"`
	// UsageURL is the endpoint read for per-window utilization. Every seat's
	// OAuth bearer token goes to it, so it is https, or http to a loopback
	// host.
	UsageURL string `yaml:"usage-url" json:"usage_url"`
	// PersistHistory writes the utilization history to disk between polls,
	// so a chart survives a host restart. The file holds utilization by
	// credential id and nothing else.
	PersistHistory bool `yaml:"persist-history" json:"persist_history"`
}

// WebConfig governs the built-in status app.
type WebConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// HistoryLimit caps retained routing decisions, at most 10000.
	HistoryLimit int `yaml:"history-limit" json:"history_limit"`
}

// DefaultUsageURL reports per-window utilization for a subscription credential
// without consuming message quota.
const DefaultUsageURL = "https://api.anthropic.com/api/oauth/usage"

// Defaults returns a config that is safe to run unattended: stickiness on, and
// a linear pace curve landing past full so a credential near its reset spends
// the budget that is about to expire.
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
			Shape:            ShapeLinear,
			CurveExponent:    1.0,
			Steepness:        8.0,
			LandingTarget:    1.10,
			WeeklyWeight:     1.0,
			SessionWeight:    0,
			ScopedWeight:     0.5,
			HysteresisMargin: 0.05,
		},
		Quota: QuotaConfig{
			PollInterval:   2 * time.Minute,
			RequestTimeout: 10 * time.Second,
			MaxStaleness:   15 * time.Minute,
			UsageURL:       DefaultUsageURL,
			PersistHistory: true,
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
		{&c.Pace.Steepness, d.Pace.Steepness},
		{&c.Pace.LandingTarget, d.Pace.LandingTarget},
		{&c.Pace.WeeklyWeight, d.Pace.WeeklyWeight},
		{&c.Pace.SessionWeight, d.Pace.SessionWeight},
		{&c.Pace.ScopedWeight, d.Pace.ScopedWeight},
		{&c.Pace.HysteresisMargin, d.Pace.HysteresisMargin},
	} {
		if math.IsNaN(*f.v) || math.IsInf(*f.v, 0) {
			*f.v = f.def
		}
	}
	if len(c.Providers) == 0 {
		c.Providers = d.Providers
	}
	c.Providers = foldKeys(c.Providers)
	c.Models = foldKeys(c.Models)
	if c.Affinity.TTL <= 0 {
		c.Affinity.TTL = d.Affinity.TTL
	}
	if c.Affinity.MaxSessions <= 0 {
		c.Affinity.MaxSessions = d.Affinity.MaxSessions
	}
	if c.Pace.CurveExponent <= 0 {
		c.Pace.CurveExponent = d.Pace.CurveExponent
	}
	// A landing above full is the use-it-or-lose-it tilt and is admissible;
	// Target's clamp is what bounds it. Past 4 the curve aims at four times a
	// budget no window can hold, which reads as a misconfiguration rather than
	// a policy whatever the shape; how much of the window the clamp then flattens
	// depends on Shape, so no single fraction describes it.
	if c.Pace.LandingTarget <= 0 || c.Pace.LandingTarget > 4 {
		c.Pace.LandingTarget = d.Pace.LandingTarget
	}
	if c.Pace.Steepness <= 0 {
		c.Pace.Steepness = d.Pace.Steepness
	}
	// A negative weight would invert the pace preference, sending work to the
	// credential furthest over its target. Zero is meaningful — it retires a
	// window from the score without retiring its gates — so only the sign is
	// corrected. Weights are relative, so 100 leaves ample range; far past it a
	// weighted slack overflows to an infinite cost, which JSON cannot encode.
	for _, w := range []struct {
		v   *float64
		def float64
	}{
		{&c.Pace.WeeklyWeight, d.Pace.WeeklyWeight},
		{&c.Pace.SessionWeight, d.Pace.SessionWeight},
		{&c.Pace.ScopedWeight, d.Pace.ScopedWeight},
	} {
		switch {
		case *w.v < 0:
			*w.v = 0
		case *w.v > 100:
			*w.v = w.def
		}
	}
	switch shape := strings.ToLower(strings.TrimSpace(c.Pace.Shape)); {
	case shape == ShapeLinear || shape == ShapePower || shape == ShapeSigmoid:
		c.Pace.Shape = shape
	case shape == "" && c.Pace.CurveExponent != d.Pace.CurveExponent:
		// Only the power shape reads an exponent, so an exponent set with no
		// shape named names the power shape.
		c.Pace.Shape = ShapePower
	default:
		c.Pace.Shape = d.Pace.Shape
	}
	if c.Pace.HysteresisMargin < 0 {
		c.Pace.HysteresisMargin = 0
	}
	if c.Quota.PollInterval < 30*time.Second {
		c.Quota.PollInterval = d.Quota.PollInterval
	}
	// One poll shares a fixed budget across every credential, so a fetch
	// allowed to hang past a minute can spend it alone.
	if c.Quota.RequestTimeout <= 0 || c.Quota.RequestTimeout > time.Minute {
		c.Quota.RequestTimeout = d.Quota.RequestTimeout
	}
	if c.Quota.MaxStaleness <= 0 {
		c.Quota.MaxStaleness = d.Quota.MaxStaleness
	}
	if !trustedUsageURL(c.Quota.UsageURL) {
		c.Quota.UsageURL = d.Quota.UsageURL
	}
	// The decision log allocates every slot up front, so a cap in the
	// billions is an allocation no recover survives.
	if c.Web.HistoryLimit <= 0 || c.Web.HistoryLimit > 10000 {
		c.Web.HistoryLimit = d.Web.HistoryLimit
	}
}

// trustedUsageURL reports whether a usage URL may receive every seat's OAuth
// bearer token: https to any host, or plain http to a loopback host only.
func trustedUsageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// foldKeys lowercases and trims every entry and drops the blanks. Provider and
// model keys are case-insensitive identifiers, and the callers that match
// against them compare lowercase, so a config written "Claude" has to fold to
// the key the host uses or it matches nothing.
func foldKeys(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// GovernsProvider reports whether the plugin should decide for a provider.
// The argument is a lowercase provider key, matching what Normalize folded
// Providers to.
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
func (c Config) GovernsModel(modelID string) bool {
	if len(c.Models) == 0 {
		return true
	}
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	for _, m := range c.Models {
		if m == modelID {
			return true
		}
	}
	return false
}
