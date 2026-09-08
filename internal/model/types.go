// Package model holds the domain types shared by the quota, pace, session,
// runtime, and web packages. It depends on nothing else in this module so any
// package may import it without creating a cycle.
package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// WindowKind identifies which Anthropic rate-limit window a reading describes.
type WindowKind string

const (
	// WindowSession is the rolling 5-hour session window.
	WindowSession WindowKind = "five_hour"
	// WindowWeekly is the 7-day all-models window. Its reset is a fixed
	// per-account anchor rather than a rolling offset from first use.
	WindowWeekly WindowKind = "seven_day"
	// WindowWeeklyScoped is a model-family weekly cap (Opus, Sonnet, Fable).
	// Scope carries the family name.
	WindowWeeklyScoped WindowKind = "weekly_scoped"
)

// Model families. The usage endpoint names them in scope.model.display_name
// and WindowWeeklyScoped.Scope carries the same spelling, so both parsers and
// the scorer key on these exact strings.
const (
	FamilyOpus   = "Opus"
	FamilySonnet = "Sonnet"
	FamilyFable  = "Fable"
	FamilyHaiku  = "Haiku"
)

// Families lists every known family, in the order a model id is matched
// against them.
var Families = []string{FamilyOpus, FamilySonnet, FamilyFable, FamilyHaiku}

// FamilyOf folds a model id, a usage-endpoint display name such as
// "Claude Sonnet 4.5", or a header claim such as "seven_day_opus" onto its
// family. It matches case-insensitively on the family token and returns ""
// when no family matches, in which case the model has no scoped window.
func FamilyOf(s string) string {
	lower := strings.ToLower(s)
	for _, f := range Families {
		if strings.Contains(lower, strings.ToLower(f)) {
			return f
		}
	}
	return ""
}

// Window durations. Anthropic publishes no absolute token caps, so a window is
// only ever expressed as a utilization fraction plus a reset instant, and the
// duration is needed to convert a reset instant into elapsed fraction.
const (
	SessionDuration = 5 * time.Hour
	WeeklyDuration  = 7 * 24 * time.Hour
)

// Status values from the Anthropic-Ratelimit-Unified-*-Status headers.
const (
	StatusAllowed        = "allowed"
	StatusAllowedWarning = "allowed_warning"
	StatusRejected       = "rejected"
)

// Severity values from the /api/oauth/usage limits[] array.
const (
	SeverityNormal   = "normal"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Window is one rate-limit window reading for one credential.
//
// Utilization is normalized to 0..1 regardless of source: the usage endpoint
// reports 0..100 while the response headers report 0..1. Values above 1 are
// legitimate and occur when usage runs past a window's cap.
type Window struct {
	Kind        WindowKind    `json:"kind"`
	Scope       string        `json:"scope,omitempty"`
	Utilization float64       `json:"utilization"`
	ResetsAt    time.Time     `json:"resets_at"`
	Duration    time.Duration `json:"duration"`
	Status      string        `json:"status,omitempty"`
	Severity    string        `json:"severity,omitempty"`
	// Active marks the window the provider currently treats as binding.
	Active bool `json:"active"`
}

// Elapsed is the fraction of the window that has passed, clamped to 0..1. A
// zero ResetsAt means no window is open, which reports 0.
func (w Window) Elapsed(now time.Time) float64 {
	if w.ResetsAt.IsZero() || w.Duration <= 0 {
		return 0
	}
	remaining := w.ResetsAt.Sub(now)
	if remaining <= 0 {
		return 1
	}
	if remaining >= w.Duration {
		return 0
	}
	return 1 - remaining.Seconds()/w.Duration.Seconds()
}

// Blocking reports whether the provider has already refused this window.
func (w Window) Blocking() bool {
	return w.Status == StatusRejected || w.Severity == SeverityCritical
}

// BearsOn reports whether this window caps a request for a model family. The
// session and all-models weekly windows cap every request; a scoped weekly
// window caps only its own family, and an empty family names no scoped window,
// so every scoped cap is another family's.
//
// This is the definition of "does this cap apply to this model": the scorer,
// the pick and the status app all answer it from here.
func (w Window) BearsOn(family string) bool {
	switch w.Kind {
	case WindowSession, WindowWeekly:
		return true
	case WindowWeeklyScoped:
		return family != "" && FamilyOf(w.Scope) == family
	}
	return false
}

// Sample is one utilization observation of one window. It travels as the
// two-element array [unix_seconds, utilization], which is a third of the size
// of an object with two keys over the thousands of samples a status response
// carries.
type Sample struct {
	At          time.Time
	Utilization float64
}

// MarshalJSON writes [unix_seconds, utilization].
func (s Sample) MarshalJSON() ([]byte, error) {
	if math.IsNaN(s.Utilization) || math.IsInf(s.Utilization, 0) {
		return nil, fmt.Errorf("sample utilization %v is not finite", s.Utilization)
	}
	return []byte("[" + strconv.FormatInt(s.At.Unix(), 10) + "," + strconv.FormatFloat(s.Utilization, 'g', -1, 64) + "]"), nil
}

// UnmarshalJSON reads the array MarshalJSON writes. The instant is UTC at
// second precision, which is the precision the array carries.
func (s *Sample) UnmarshalJSON(b []byte) error {
	var raw [2]float64
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.At = time.Unix(int64(raw[0]), 0).UTC()
	s.Utilization = raw[1]
	return nil
}

// Cycle is one pass through a window: the samples observed while the window
// carried one reset instant. A window that rolls over starts a new cycle, so
// the boundary between two cycles is where a chart breaks its line rather than
// drawing utilization falling back to zero. ResetsAt is zero for a cycle whose
// readings never carried a reset.
type Cycle struct {
	ResetsAt time.Time `json:"resets_at"`
	Samples  []Sample  `json:"samples"`
}

// WindowHistory is the recorded utilization of one window of one credential,
// oldest cycle first and oldest sample first within a cycle. It carries no
// identity beyond the window's own, so the unauthenticated status route can
// publish it as is.
type WindowHistory struct {
	Kind   WindowKind `json:"kind"`
	Scope  string     `json:"scope,omitempty"`
	Cycles []Cycle    `json:"cycles"`
}

// Samples counts the samples across every cycle.
func (h WindowHistory) Samples() int {
	n := 0
	for _, c := range h.Cycles {
		n += len(c.Samples)
	}
	return n
}

// Source identifies where a snapshot's readings came from.
const (
	// SourceUsageEndpoint is an active GET of /api/oauth/usage. It covers
	// every window including those for models the credential has not served.
	SourceUsageEndpoint = "usage-endpoint"
	// SourceResponseHeaders is a passive read of the unified rate-limit
	// headers on a real response. Free, but only for windows that traffic
	// has actually touched.
	SourceResponseHeaders = "response-headers"
)

// AuthSnapshot is the most recent quota reading for one credential.
type AuthSnapshot struct {
	AuthID     string    `json:"auth_id"`
	AuthIndex  string    `json:"auth_index,omitempty"`
	Label      string    `json:"label,omitempty"`
	Windows    []Window  `json:"windows"`
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
	// Err records the last fetch failure. A snapshot with Err set retains
	// whatever readings it already held.
	Err string `json:"err,omitempty"`
	// ErrCategory is the machine-readable class of Err, such as auth or
	// timeout, so the status app can render it without parsing the message.
	ErrCategory string `json:"err_category,omitempty"`
}

// Window returns the reading for a kind and scope, and whether it exists.
func (s AuthSnapshot) Window(kind WindowKind, scope string) (Window, bool) {
	for _, w := range s.Windows {
		if w.Kind == kind && w.Scope == scope {
			return w, true
		}
	}
	return Window{}, false
}

// Stale reports whether the snapshot is older than the given bound.
func (s AuthSnapshot) Stale(now time.Time, maxAge time.Duration) bool {
	return s.ObservedAt.IsZero() || now.Sub(s.ObservedAt) > maxAge
}

// WindowScore is the pace evaluation of one window.
type WindowScore struct {
	Kind  WindowKind `json:"kind"`
	Scope string     `json:"scope,omitempty"`
	// Elapsed is the fraction of the window that has passed.
	Elapsed float64 `json:"elapsed"`
	// Target is the utilization the pace curve expects at Elapsed.
	Target float64 `json:"target"`
	// Utilization is the observed fraction consumed.
	Utilization float64 `json:"utilization"`
	// Slack is Target-Utilization. Positive means under-consumed relative to
	// pace, and therefore preferred.
	Slack float64 `json:"slack"`
	// Weight is the coefficient this window contributes with.
	Weight float64 `json:"weight"`
	// ResetsAt is carried through so the timeline view can plot the window
	// without a second lookup.
	ResetsAt time.Time `json:"resets_at"`
}

// Ineligibility reasons.
const (
	ReasonEligible   = ""
	ReasonHardCutoff = "hard-cutoff"
	ReasonRejected   = "provider-rejected"
	ReasonNoSnapshot = "no-snapshot"
	// ReasonNoWindow marks a snapshot that exists but has no window bearing on
	// the requested model, such as one holding only another family's cap.
	ReasonNoWindow = "no-bearing-window"
	// ReasonBadReading marks a snapshot whose utilization is not a finite
	// number, which no comparison can order safely.
	ReasonBadReading   = "bad-reading"
	ReasonStale        = "stale-snapshot"
	ReasonNotCandidate = "not-a-candidate"
)

// Score is the routing evaluation of one credential for one request.
type Score struct {
	AuthID string `json:"auth_id"`
	// Total is the weighted sum of window slacks less the raw-utilization
	// penalty. Higher wins.
	Total   float64       `json:"total"`
	Windows []WindowScore `json:"windows"`
	// RawPenalty is the load-balancing term. Pace slack alone under-penalizes
	// a heavily used credential that happens to be on pace, which starves
	// idle siblings.
	RawPenalty float64 `json:"raw_penalty"`
	Eligible   bool    `json:"eligible"`
	Reason     string  `json:"reason,omitempty"`
}

// Decision kinds.
const (
	// DecisionAffinityHit reuses an existing session binding.
	DecisionAffinityHit = "affinity-hit"
	// DecisionColdPick selects a credential for a session with no binding.
	DecisionColdPick = "cold-pick"
	// DecisionFailover rebinds because the bound credential is not among the
	// candidates the host offered.
	DecisionFailover = "failover"
	// DecisionDeclined hands the choice back to the host's own selector.
	DecisionDeclined = "declined"
)

// Decision records one routing outcome for the status and timeline views.
type Decision struct {
	At             time.Time `json:"at"`
	SessionKey     string    `json:"session_key,omitempty"`
	Model          string    `json:"model"`
	Provider       string    `json:"provider"`
	ChosenAuthID   string    `json:"chosen_auth_id,omitempty"`
	PreviousAuthID string    `json:"previous_auth_id,omitempty"`
	Kind           string    `json:"kind"`
	// Note explains a declined or unusual decision.
	Note string `json:"note,omitempty"`
	// Scores holds the evaluation of every candidate, so the UI can show why
	// the winner won.
	Scores []Score `json:"scores,omitempty"`
	// Subagent marks a request routed as a child of another session.
	Subagent bool `json:"subagent"`
}

// Binding pins one conversation to one credential so Anthropic prompt caches
// keep hitting. Caches are isolated between organizations, so moving a live
// conversation to another credential guarantees a full cache miss.
//
// A binding is scoped per provider and model as well as per conversation,
// because prompt caches are per model and different models may be served by
// different credential sets. SessionKey is the bare conversation key, the same
// value Decision.SessionKey carries.
type Binding struct {
	SessionKey string    `json:"session_key"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	AuthID     string    `json:"auth_id"`
	BoundAt    time.Time `json:"bound_at"`
	LastSeen   time.Time `json:"last_seen"`
	Hits       int       `json:"hits"`
}

// CacheStats accumulates the cache-token counters reported by the host for one
// credential, which is how the plugin measures whether its routing is actually
// preserving caches.
type CacheStats struct {
	Requests            int64 `json:"requests"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	FreshInputTokens    int64 `json:"fresh_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
}

// HitRate is the share of input-side tokens served from cache, or 0 when no
// input-side tokens have been recorded.
func (c CacheStats) HitRate() float64 {
	total := c.CacheReadTokens + c.CacheCreationTokens + c.FreshInputTokens
	if total <= 0 {
		return 0
	}
	return float64(c.CacheReadTokens) / float64(total)
}

// PluginInfo identifies the running plugin build and its host to the status
// app.
type PluginInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// HostSchemaVersion is the RPC contract version the host announced at
	// registration.
	HostSchemaVersion uint32    `json:"host_schema_version"`
	StartedAt         time.Time `json:"started_at"`
}

// AuthStatus is one credential's row in the status app: the host's view of
// it, its latest quota reading, and its pace evaluation for one model.
type AuthStatus struct {
	AuthID string `json:"auth_id"`
	// Label is the host's operator-facing name: the credential's label field,
	// else its account email. Two credentials of one account share it.
	Label string `json:"label,omitempty"`
	// Name is the credential file name as the host lists it. Files in one
	// directory are distinct, so it tells two credentials of one account
	// apart where Label cannot.
	Name string `json:"name,omitempty"`
	// Email is the account address the host reads off the credential.
	Email    string `json:"email,omitempty"`
	Provider string `json:"provider,omitempty"`
	Priority int    `json:"priority"`
	// HostStatus is the credential state the host reports, such as active,
	// disabled or cooling.
	HostStatus string       `json:"host_status,omitempty"`
	Snapshot   AuthSnapshot `json:"snapshot"`
	// Score is the pace evaluation for Status.Model at Status.Now.
	Score Score `json:"score"`
	// Bindings counts live conversations pinned to this credential.
	Bindings int        `json:"bindings"`
	Cache    CacheStats `json:"cache"`
	// History is the recorded utilization of each window, thinned to what a
	// chart can draw. Utilization only: no identity travels in it.
	History []WindowHistory `json:"history"`
}

// Status is the complete state the status app renders. It carries no
// credential material: labels and ids only.
type Status struct {
	Now    time.Time  `json:"now"`
	Plugin PluginInfo `json:"plugin"`
	Config Config     `json:"config"`
	// Model is the model id every AuthStatus.Score was evaluated for.
	Model     string       `json:"model"`
	Auths     []AuthStatus `json:"auths"`
	Bindings  []Binding    `json:"bindings"`
	Decisions []Decision   `json:"decisions"`
	// Warnings are operator-facing conditions that leave the plugin inert or
	// degraded, such as a single-candidate pool or host session affinity
	// still enabled.
	Warnings []string `json:"warnings"`
}
