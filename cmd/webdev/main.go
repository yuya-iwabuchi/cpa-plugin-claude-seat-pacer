// Command webdev serves the embedded status app against a fixture Source so
// the page can be developed and screenshotted without a CLIProxyAPI host.
//
// It is a development harness: it binds to loopback only and never reads a
// real credential.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/pace"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/web"
)

func main() {
	port := flag.Int("port", 8377, "loopback port to serve the status app on")
	scenario := flag.String("scenario", "full",
		"fixture scenario: full, single, stale, degraded or empty")
	latency := flag.Duration("latency", 0, "delay every status response, to see the loading state")
	failAfter := flag.Int("fail-after", -1,
		"fail status requests after this many successes; 0 fails the first, -1 never fails")
	flag.Parse()

	src := newFixture(time.Now())
	switch *scenario {
	case "single":
		src.auths = src.auths[:1]
	case "stale":
		// Past quota.max-staleness the plugin declines to evaluate the
		// credential at all, which is the state the lanes render as unknown.
		src.observedAge[seatBID] = src.cfg.Quota.MaxStaleness + 5*time.Minute
	case "degraded":
		// The warnings a healthy pool never raises: the plugin switched off,
		// the credential listing failing, and a credential the poller has
		// never published a reading for.
		src.cfg.Enabled = false
		src.listErr = "read auth dir: permission denied"
		src.auths = append(src.auths, model.AuthStatus{
			AuthID: seatCID, Label: "Seat C", Provider: "claude",
			Priority: 10, HostStatus: "active",
		})
	case "empty":
		src.auths = nil
		src.bindings = nil
		src.decisions = nil
	case "full":
	default:
		log.Fatalf("unknown scenario %q", *scenario)
	}
	src.rebuildWarnings()

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           faults(web.NewHandler(src), *latency, *failAfter),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("status app on http://%s/", addr)
	log.Fatal(srv.ListenAndServe())
}

// faults injects the transport conditions the page has to survive: a slow
// status endpoint, and one that starts failing while the page is open.
func faults(h http.Handler, latency time.Duration, failAfter int) http.Handler {
	var served atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			if latency > 0 {
				time.Sleep(latency)
			}
			if failAfter >= 0 && served.Add(1) > int64(failAfter) {
				http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

const (
	seatAID = "claude-oauth-a4f1c2"
	seatBID = "claude-oauth-9b30de"
	seatCID = "claude-oauth-71c8fa"

	// seatALabel is what the host reports for a credential that names no label
	// of its own: the account address. It sits on the leading seat, whose card
	// also carries the widest rank badge.
	seatALabel = "quota.ops@acme-corp-engineering.example"

	modelFable  = "claude-fable-5-20260514"
	modelOpus   = "claude-opus-4-6-20260212"
	modelSonnet = "claude-sonnet-4-5-20250929"
)

// fixture is a Source with a fixed scenario. Reset instants are anchored to
// process start, so the page's countdowns run for real while it is open.
type fixture struct {
	anchor    time.Time
	cfg       model.Config
	plugin    model.PluginInfo
	snapshots map[string]model.AuthSnapshot
	// observedAge is how far behind the request each credential's reading is.
	// Status stamps ObservedAt from it, so a harness left open overnight keeps
	// serving fresh snapshots the way a working poller does.
	observedAge map[string]time.Duration
	// snapshotsPast holds the readings that were current earlier in the log.
	snapshotsPast map[string]model.AuthSnapshot
	auths         []model.AuthStatus
	bindings      []model.Binding
	decisions     []model.Decision
	// listErr is the host credential listing's last failure, empty while it
	// succeeds.
	listErr  string
	warnings []string
}

func newFixture(anchor time.Time) *fixture {
	cfg := model.Defaults()
	// Defaults leave the plugin off, and a status view of a plugin that routes
	// nothing is a different page.
	cfg.Enabled = true
	// A conservative curve: the target trails elapsed early and lands short of
	// full, which separates the pace tick from the now line in the timeline.
	cfg.Pace.CurveExponent = 1.35
	cfg.Pace.LandingTarget = 0.95

	f := &fixture{
		anchor: anchor,
		cfg:    cfg,
		plugin: model.PluginInfo{
			Name:              "cpa-claude-quota-scheduler",
			Version:           "0.4.2+dev",
			HostSchemaVersion: 1,
			StartedAt:         anchor.Add(-6*time.Hour - 13*time.Minute),
		},
		observedAge: map[string]time.Duration{
			seatAID: 42 * time.Second,
			seatBID: 3*time.Minute + 10*time.Second,
		},
	}

	f.snapshots = map[string]model.AuthSnapshot{
		seatAID: {
			AuthID:    seatAID,
			AuthIndex: "0",
			Label:     seatALabel,
			Source:    model.SourceUsageEndpoint,
			Windows: []model.Window{
				{
					Kind: model.WindowSession, Utilization: 0.80,
					ResetsAt: anchor.Add(2*time.Hour + 5*time.Minute),
					Duration: model.SessionDuration,
					Status:   model.StatusAllowed, Severity: model.SeverityNormal, Active: true,
				},
				{
					Kind: model.WindowWeekly, Utilization: 0.04,
					ResetsAt: anchor.Add(7 * 24 * time.Hour),
					Duration: model.WeeklyDuration,
					Status:   model.StatusAllowed, Severity: model.SeverityNormal,
				},
				{
					Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0.0,
					ResetsAt: anchor.Add(5*24*time.Hour + 12*time.Hour),
					Duration: model.WeeklyDuration,
					Status:   model.StatusAllowed, Severity: model.SeverityNormal,
				},
			},
		},
		seatBID: {
			AuthID:      seatBID,
			AuthIndex:   "1",
			Label:       "Seat B",
			Source:      model.SourceResponseHeaders,
			Err:         "Get \"https://api.anthropic.com/api/oauth/usage\": context deadline exceeded",
			ErrCategory: "timeout",
			Windows: []model.Window{
				{
					Kind: model.WindowSession, Utilization: 1.00,
					ResetsAt: anchor.Add(2*time.Hour + 30*time.Minute),
					Duration: model.SessionDuration,
					Status:   model.StatusRejected, Severity: model.SeverityCritical, Active: true,
				},
				{
					Kind: model.WindowWeekly, Utilization: 0.54,
					ResetsAt: anchor.Add(10 * time.Hour),
					Duration: model.WeeklyDuration,
					Status:   model.StatusAllowedWarning, Severity: model.SeverityWarning,
				},
				{
					Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0.67,
					ResetsAt: anchor.Add(3 * 24 * time.Hour),
					Duration: model.WeeklyDuration,
					Status:   model.StatusAllowed, Severity: model.SeverityNormal,
				},
			},
		},
	}

	f.auths = []model.AuthStatus{
		{
			AuthID: seatAID, Label: seatALabel, Provider: "claude", Priority: 10,
			HostStatus: "active", Bindings: 4,
			Cache: model.CacheStats{
				Requests:            1412,
				CacheReadTokens:     9_600_000,
				CacheCreationTokens: 250_000,
				FreshInputTokens:    150_000,
				OutputTokens:        318_400,
			},
		},
		{
			AuthID: seatBID, Label: "Seat B", Provider: "claude", Priority: 10,
			HostStatus: "active", Bindings: 2,
			Cache: model.CacheStats{
				Requests:            684,
				CacheReadTokens:     7_000_000,
				CacheCreationTokens: 1_800_000,
				FreshInputTokens:    1_200_000,
				OutputTokens:        204_900,
			},
		},
	}

	f.bindings = []model.Binding{
		{SessionKey: "5f2c0b7d4a19e83c", Provider: "claude", Model: modelFable, AuthID: seatAID,
			BoundAt: anchor.Add(-38 * time.Minute), LastSeen: anchor.Add(-24 * time.Second), Hits: 214},
		{SessionKey: "a91d33e0c7b45f28", Provider: "claude", Model: modelFable, AuthID: seatAID,
			BoundAt: anchor.Add(-2*time.Hour - 12*time.Minute), LastSeen: anchor.Add(-3 * time.Minute), Hits: 87},
		{SessionKey: "c4408b1ef6d92a70", Provider: "claude", Model: modelOpus, AuthID: seatBID,
			BoundAt: anchor.Add(-51 * time.Minute), LastSeen: anchor.Add(-6 * time.Minute), Hits: 63},
		{SessionKey: "1de77a2b98c30541", Provider: "claude", Model: modelSonnet, AuthID: seatAID,
			BoundAt: anchor.Add(-14 * time.Minute), LastSeen: anchor.Add(-70 * time.Second), Hits: 19},
		{SessionKey: "77b0fe4c1a8d6392", Provider: "claude", Model: modelFable, AuthID: seatBID,
			BoundAt: anchor.Add(-4*time.Hour - 5*time.Minute), LastSeen: anchor.Add(-22 * time.Minute), Hits: 341},
		{SessionKey: "e30c95814bf7a2d6", Provider: "claude", Model: modelOpus, AuthID: seatAID,
			BoundAt: anchor.Add(-9 * time.Minute), LastSeen: anchor.Add(-11 * time.Second), Hits: 7},
	}

	// Seat B only ran out of its session window part way through the log, so
	// older decisions score against the readings that were current then.
	f.snapshotsPast = map[string]model.AuthSnapshot{
		seatAID: withSessionUtil(f.snapshots[seatAID], 0.31, model.StatusAllowed, model.SeverityNormal),
		seatBID: withSessionUtil(f.snapshots[seatBID], 0.42, model.StatusAllowed, model.SeverityNormal),
	}

	f.decisions = f.buildDecisions()
	return f
}

// withSessionUtil copies a snapshot with a different 5-hour reading.
func withSessionUtil(s model.AuthSnapshot, util float64, status, severity string) model.AuthSnapshot {
	out := s
	out.Windows = append([]model.Window(nil), s.Windows...)
	for i := range out.Windows {
		if out.Windows[i].Kind == model.WindowSession {
			out.Windows[i].Utilization = util
			out.Windows[i].Status = status
			out.Windows[i].Severity = severity
		}
	}
	return out
}

// rebuildWarnings restates every warning runtime.Status emits, for whichever
// credentials and config the scenario leaves in place. Nothing warns about the
// host's own routing.session-affinity: no signal the plugin receives
// distinguishes it.
func (f *fixture) rebuildWarnings() {
	f.warnings = nil
	if !f.cfg.Enabled {
		f.warnings = append(f.warnings,
			"plugin is disabled by configuration; the host's own selector routes every request")
	}
	if f.listErr != "" {
		f.warnings = append(f.warnings, "credential listing is failing: "+f.listErr)
	}
	if len(f.auths) == 1 {
		f.warnings = append(f.warnings, fmt.Sprintf("provider %s offered a single candidate; "+
			"the pool shares one priority tier, so the rest are unavailable to the host or already rejected upstream", "claude"))
	}
	for _, a := range f.auths {
		snap, ok := f.snapshots[a.AuthID]
		if !ok {
			f.warnings = append(f.warnings, fmt.Sprintf(
				"no quota snapshot for %s; it is ineligible for cold picks", a.AuthID))
			continue
		}
		if snap.Err != "" {
			f.warnings = append(f.warnings, fmt.Sprintf("quota poll failing for %s (%s): %s",
				a.AuthID, snap.ErrCategory, snap.Err))
		}
	}
}

// scoreWithStaleness mirrors the gates internal/runtime applies before scoring:
// a credential with no reading, or one older than quota.max-staleness, is not
// evaluated at all, and the score carries the reason instead of a window
// breakdown.
func scoreWithStaleness(cfg model.Config, snap model.AuthSnapshot, hasSnap bool, modelID string, now time.Time) model.Score {
	switch {
	case !hasSnap:
		return model.Score{AuthID: snap.AuthID, Reason: model.ReasonNoSnapshot}
	case snap.Stale(now, cfg.Quota.MaxStaleness):
		return model.Score{AuthID: snap.AuthID, Reason: model.ReasonStale}
	}
	return pace.ScoreAuth(cfg.Pace, snap, modelID, now)
}

// Status evaluates both fixture credentials for modelID at now.
func (f *fixture) Status(now time.Time, modelID string) model.Status {
	if modelID == "" {
		modelID = modelFable
	}
	auths := make([]model.AuthStatus, 0, len(f.auths))
	for _, a := range f.auths {
		snap, ok := f.snapshots[a.AuthID]
		snap.AuthID = a.AuthID
		if ok {
			snap.ObservedAt = now.Add(-f.observedAge[a.AuthID])
		}
		a.Snapshot = snap
		a.Score = scoreWithStaleness(f.cfg, snap, ok, modelID, now)
		a.Score.AuthID = a.AuthID
		auths = append(auths, a)
	}
	return model.Status{
		Now:       now,
		Plugin:    f.plugin,
		Config:    f.cfg,
		Model:     modelID,
		Auths:     auths,
		Bindings:  f.bindings,
		Decisions: f.decisions,
		Warnings:  f.warnings,
	}
}

// decisionSpec is one scripted routing outcome, offset back from the anchor.
type decisionSpec struct {
	ago      time.Duration
	kind     string
	key      string
	modelID  string
	chosen   string
	previous string
	note     string
	subagent bool
	scored   bool
}

func (f *fixture) buildDecisions() []model.Decision {
	specs := []decisionSpec{
		{ago: 24 * time.Second, kind: model.DecisionAffinityHit, key: "5f2c0b7d4a19e83c", modelID: modelFable, chosen: seatAID},
		{ago: 41 * time.Second, kind: model.DecisionColdPick, key: "b18e4402fd7c9a35", modelID: modelFable, chosen: seatAID, scored: true},
		{ago: 70 * time.Second, kind: model.DecisionAffinityHit, key: "1de77a2b98c30541", modelID: modelSonnet, chosen: seatAID},
		{ago: 96 * time.Second, kind: model.DecisionAffinityHit, key: "5f2c0b7d4a19e83c", modelID: modelFable, chosen: seatAID, subagent: true},
		{ago: 2 * time.Minute, kind: model.DecisionFailover, key: "c4408b1ef6d92a70", modelID: modelOpus, chosen: seatAID, previous: seatBID,
			note: "bound credential was not among the candidates the host offered", scored: true},
		{ago: 3 * time.Minute, kind: model.DecisionAffinityHit, key: "a91d33e0c7b45f28", modelID: modelFable, chosen: seatAID},
		{ago: 4 * time.Minute, kind: model.DecisionColdPick, key: "6c2a90f31be8d574", modelID: modelOpus, chosen: seatAID, scored: true},
		{ago: 5 * time.Minute, kind: model.DecisionAffinityHit, key: "77b0fe4c1a8d6392", modelID: modelFable, chosen: seatBID},
		{ago: 6 * time.Minute, kind: model.DecisionAffinityHit, key: "c4408b1ef6d92a70", modelID: modelOpus, chosen: seatBID},
		{ago: 7 * time.Minute, kind: model.DecisionDeclined, key: "", modelID: "gemini-2.5-pro",
			note: "provider is not governed by this plugin"},
		{ago: 8 * time.Minute, kind: model.DecisionAffinityHit, key: "5f2c0b7d4a19e83c", modelID: modelFable, chosen: seatAID, subagent: true},
		{ago: 9 * time.Minute, kind: model.DecisionColdPick, key: "e30c95814bf7a2d6", modelID: modelOpus, chosen: seatAID, scored: true},
		{ago: 11 * time.Minute, kind: model.DecisionAffinityHit, key: "a91d33e0c7b45f28", modelID: modelFable, chosen: seatAID},
		{ago: 12 * time.Minute, kind: model.DecisionDeclined, key: "0a5f7ce2b4318d9f", modelID: modelFable,
			note: "every snapshot was older than max-staleness"},
		{ago: 13 * time.Minute, kind: model.DecisionAffinityHit, key: "77b0fe4c1a8d6392", modelID: modelFable, chosen: seatBID},
		{ago: 14 * time.Minute, kind: model.DecisionColdPick, key: "1de77a2b98c30541", modelID: modelSonnet, chosen: seatAID, scored: true},
		{ago: 16 * time.Minute, kind: model.DecisionAffinityHit, key: "c4408b1ef6d92a70", modelID: modelOpus, chosen: seatBID, subagent: true},
		{ago: 18 * time.Minute, kind: model.DecisionAffinityHit, key: "5f2c0b7d4a19e83c", modelID: modelFable, chosen: seatAID},
		{ago: 20 * time.Minute, kind: model.DecisionFailover, key: "3b7fa1c05e29d846", modelID: modelFable, chosen: seatAID, previous: seatBID,
			note: "the bound credential rejected the 5-hour window", scored: true},
		{ago: 22 * time.Minute, kind: model.DecisionAffinityHit, key: "77b0fe4c1a8d6392", modelID: modelFable, chosen: seatBID},
		{ago: 24 * time.Minute, kind: model.DecisionColdPick, key: "d2f8410ba36c7e15", modelID: modelFable, chosen: seatBID, scored: true},
		{ago: 26 * time.Minute, kind: model.DecisionAffinityHit, key: "a91d33e0c7b45f28", modelID: modelFable, chosen: seatAID, subagent: true},
		{ago: 28 * time.Minute, kind: model.DecisionAffinityHit, key: "6c2a90f31be8d574", modelID: modelOpus, chosen: seatAID},
		{ago: 31 * time.Minute, kind: model.DecisionDeclined, key: "9f01c73de5a8b204", modelID: modelFable,
			note: "the host offered a single candidate; nothing to spread across"},
		{ago: 34 * time.Minute, kind: model.DecisionColdPick, key: "8ac41f60d97b2e53", modelID: modelSonnet, chosen: seatBID, scored: true},
		{ago: 36 * time.Minute, kind: model.DecisionAffinityHit, key: "5f2c0b7d4a19e83c", modelID: modelFable, chosen: seatAID},
		{ago: 38 * time.Minute, kind: model.DecisionColdPick, key: "5f2c0b7d4a19e83c", modelID: modelFable, chosen: seatAID, scored: true},
		{ago: 42 * time.Minute, kind: model.DecisionAffinityHit, key: "77b0fe4c1a8d6392", modelID: modelFable, chosen: seatBID},
		{ago: 47 * time.Minute, kind: model.DecisionAffinityHit, key: "77b0fe4c1a8d6392", modelID: modelFable, chosen: seatBID, subagent: true},
		{ago: 53 * time.Minute, kind: model.DecisionColdPick, key: "77b0fe4c1a8d6392", modelID: modelFable, chosen: seatBID, scored: true},
	}

	out := make([]model.Decision, 0, len(specs))
	ids := []string{seatAID, seatBID}
	for _, s := range specs {
		at := f.anchor.Add(-s.ago)
		d := model.Decision{
			At:             at,
			SessionKey:     s.key,
			Model:          s.modelID,
			Provider:       "claude",
			ChosenAuthID:   s.chosen,
			PreviousAuthID: s.previous,
			Kind:           s.kind,
			Note:           s.note,
			Subagent:       s.subagent,
		}
		if s.scored {
			snaps := f.snapshots
			if s.ago >= 22*time.Minute {
				snaps = f.snapshotsPast
			}
			d.Scores = pace.Rank(f.cfg.Pace, snaps, ids, s.modelID, at)
		}
		out = append(out, d)
	}
	return out
}
