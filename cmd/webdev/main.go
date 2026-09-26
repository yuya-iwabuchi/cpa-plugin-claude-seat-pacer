// Command webdev serves the embedded status app against a fixture Source, of
// synthetic seats or of a recorded history file, so the page can be developed
// and screenshotted without a CLIProxyAPI host.
//
// It is a development harness: it binds to loopback only. Only the replay
// scenario reads an install's files: its history file, the page-id.key beside
// it, and each seat's note and email from its credential file, never a token.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/pace"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/runtime"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/web"
)

func main() {
	port := flag.Int("port", 8377, "loopback port to serve the status app on")
	scenario := flag.String("scenario", "full",
		"fixture scenario: full, single, stale, degraded, many, collide, cleared, exhausted, replay or empty")
	historyPath := flag.String("history", "", "history file the replay scenario draws its seats from")
	locksPath := flag.String("locks", "",
		"lock spans file the replay scenario adds to the spans its history file records: a JSON object keyed by credential id, "+
			`each a list of {"kind", "scope", "from", "to", "cut_by_success"} with Unix-second ends`)
	at := flag.String("at", "", "instant the replay scenario renders the page as of, local time, 2006-01-02T15:04")
	seats := flag.Int("seats", 6, "credential count for the many scenario")
	latency := flag.Duration("latency", 0, "delay every status response, to see the loading state")
	failAfter := flag.Int("fail-after", -1,
		"fail status requests after this many successes; 0 fails the first, -1 never fails")
	flag.Parse()
	if *scenario != "replay" && (*historyPath != "" || *locksPath != "" || *at != "") {
		log.Fatal("-history, -locks and -at apply only to the replay scenario")
	}
	// The many scenario spreads its bindings across the seats, so it needs at
	// least one.
	if *seats < 1 {
		*seats = 1
	}

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
	case "many":
		// A pool the operator has grown past the point where every seat gets
		// a roomy row: the naming collisions a real pool produces, and every
		// lane state the page can draw, spread across the seats.
		src.growTo(*seats)
	case "collide":
		// Two seats a few hours apart in their weeks at near-equal utilization,
		// so their four pace-curve labels contend for one patch of the plot;
		// one of them is at its Fable cap with its weekly window fine, so it
		// is eligible for Standard requests and not for Fable ones.
		src.collide()
	case "cleared":
		// A seat whose provider cleared both weekly windows half a day ago and
		// kept their reset instant, so its recorded lines climb to near full,
		// fall and climb again to the reading.
		src.cleared()
	case "exhausted":
		// Every seat past its 5-hour window, with the host falling through to
		// the priority tier below: an API-key credential the poller never
		// reads, which takes the traffic until a seat resets.
		src.exhausted()
	case "replay":
		// The seats of a recorded history file, with the spans in which the
		// provider refused them.
		if *historyPath == "" {
			log.Fatal("the replay scenario needs -history")
		}
		replayNow := time.Now()
		if *at != "" {
			t, err := time.ParseInLocation("2006-01-02T15:04", *at, time.Local)
			if err != nil {
				log.Fatalf("-at: %v", err)
			}
			replayNow = t
			src.moveClock(time.Until(t))
		}
		if err := src.replay(*historyPath, *locksPath, replayNow); err != nil {
			log.Fatalf("replay: %v", err)
		}
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
		Handler:           faults(framed(web.NewHandler(src)), *latency, *failAfter),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("status app on http://%s/", addr)
	log.Fatal(srv.ListenAndServe())
}

// framePage stands in for the Management Center: it frames index.html the way
// the console does and floats a toolbar over the top-right corner of the frame,
// which is the corner the page has to keep clear. Served at /frame.html.
const framePage = `<!doctype html><meta charset="utf-8"><title>framed</title>
<style>
html,body{margin:0;height:100%;background:#eef0f4;font:13px system-ui}
.bar{height:48px;display:flex;align-items:center;padding:0 16px;background:#fff;border-bottom:1px solid #d9dce3}
.frame{position:relative;height:calc(100% - 48px)}
iframe{border:0;width:100%;height:100%;display:block}
.tools{position:absolute;top:12px;right:16px;display:flex;gap:8px}
.tools span{width:34px;height:34px;border-radius:8px;background:#fff;border:1px solid #c9cdd6;box-shadow:0 2px 6px rgba(0,0,0,.12);display:grid;place-items:center;font-size:15px}
</style>
<div class="bar">Management Center · Plugins · Claude Seat Pacer</div>
<div class="frame"><iframe src="index.html" title="plugin"></iframe>
<div class="tools"><span>&#8635;</span><span>&#127760;</span><span>&#9790;</span><span>&#8594;</span></div></div>
`

func framed(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/frame.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(framePage))
			return
		}
		h.ServeHTTP(w, r)
	})
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
	// of its own: the account address. It sits on the leading seat, whose row
	// also carries the "next cold pick" line.
	seatALabel = "quota.ops@acme-corp-engineering.example"

	// overflowKeyID is the runtime-only credential the host falls through to
	// when every seat is in cooldown. It has no file behind it, so the poller
	// skips it and the plugin holds no reading, no score and no seat row for
	// it.
	overflowKeyID = "claude:apikey:0f2c8ab41d7e"

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
	// clearedAgo is how long before the request the provider cleared a
	// seat's weekly windows without moving their reset.
	clearedAgo map[string]time.Duration
	// replayed is each seat's published history in the replay scenario, which
	// stands in for the synthesized one.
	replayed map[string][]model.WindowHistory
	// shift moves the clock every status response reads, so a replay can
	// render as of a recorded instant; moveClock sets it.
	shift time.Duration
	// forcedAt is when the last forced read landed, in Unix nanoseconds, as
	// SyncNow moves it. The polling loop's own schedule is untouched by one,
	// the way the plugin leaves its timer alone.
	forcedAt atomic.Int64
	// idKeyFile is the published-id key file, empty for a key of the
	// process's own.
	idKeyFile string
}

// SyncNow reports whether it read, and reads at most once every
// runtime.MinForcedPollGap, the plugin's own throttle on a forced read. The
// harness holds no upstream to re-read, so a read here moves only the stamp
// the page counts from.
func (f *fixture) SyncNow(context.Context) bool {
	now := time.Now().Add(f.shift)
	last := f.forcedAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < runtime.MinForcedPollGap {
		return false
	}
	f.forcedAt.Store(now.UnixNano())
	return true
}

// moveClock moves every instant the page counts from by d: each status
// response's now, the poll schedule's anchor, a forced read's stamp and the
// plugin's start.
func (f *fixture) moveClock(d time.Duration) {
	f.shift = d
	f.anchor = f.anchor.Add(d)
	f.plugin.StartedAt = f.plugin.StartedAt.Add(d)
}

// pollTimes is the poller's schedule as the harness keeps it: scheduled reads
// land on quota.poll-interval from process start, so a page left open watches
// a countdown that wraps for real. A forced read moves the last-read stamp
// without moving the next wake.
func (f *fixture) pollTimes(now time.Time) (polled, next time.Time) {
	every := f.cfg.Quota.PollInterval
	if every <= 0 {
		return now, time.Time{}
	}
	polled = now.Add(-(now.Sub(f.anchor) % every))
	next = polled.Add(every)
	if forced := f.forcedAt.Load(); forced != 0 && time.Unix(0, forced).After(polled) {
		polled = time.Unix(0, forced)
	}
	return polled, next
}

func newFixture(anchor time.Time) *fixture {
	// The pace curve is whatever Defaults ships: linear, landing past full, so
	// the target leads elapsed and reaches 100% before the window closes.
	cfg := model.Defaults()
	// Defaults leave the plugin off, and a status view of a plugin that routes
	// nothing is a different page.
	cfg.Enabled = true

	f := &fixture{
		anchor: anchor,
		cfg:    cfg,
		plugin: model.PluginInfo{
			Name:              "claude-seat-pacer",
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
				// The state the leading seat is really in when a family cap
				// runs down: the usage endpoint rates the headroom critical
				// around 90% while the provider has refused nothing, so the
				// seat keeps serving and keeps taking the picks it wins.
				{
					Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0.90,
					ResetsAt: anchor.Add(5*24*time.Hour + 12*time.Hour),
					Duration: model.WeeklyDuration,
					Status:   model.StatusAllowedWarning, Severity: model.SeverityCritical,
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
			AuthID: seatAID, Label: seatALabel, Name: "claude-" + seatALabel + ".json", Email: seatALabel,
			Provider: "claude", Priority: 10,
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
			AuthID: seatBID, Label: "Seat B", Name: "claude-seat-b.json", Provider: "claude", Priority: 10,
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

// rebuildWarnings is the warning list runtime.Status would build for whichever
// credentials and config the scenario leaves in place. A pool of one stands in
// for the single-candidate pick the plugin only learns about from the host.
// Nothing warns about the host's own routing.session-affinity: no signal the
// plugin receives distinguishes it.
func (f *fixture) rebuildWarnings() {
	rows := append([]model.AuthStatus(nil), f.auths...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].AuthID < rows[j].AuthID })
	seats := make(map[string]runtime.SeatWarningState, len(rows))
	for _, row := range rows {
		snap, ok := f.snapshots[row.AuthID]
		seats[row.AuthID] = runtime.SeatWarningState{
			Listed:       true,
			Disabled:     row.HostStatus == "disabled",
			HasSnapshot:  ok,
			PollErr:      snap.Err,
			PollCategory: snap.ErrCategory,
		}
	}
	var single []string
	if len(f.auths) == 1 {
		single = []string{"claude"}
	}
	f.warnings = runtime.Warnings(f.cfg.Enabled, nil, f.listErr, single, rows, seats)
}

// rankAt is the score list a decision carries, over the readings as they stood
// at that instant: each one is stamped with the age the fixture gives its
// credential, so the rank meets the same staleness gate the pick applies.
func (f *fixture) rankAt(snaps map[string]model.AuthSnapshot, ids []string, modelID string, at time.Time) []model.Score {
	stamped := make(map[string]model.AuthSnapshot, len(snaps))
	for id, snap := range snaps {
		snap.AuthID = id
		snap.ObservedAt = at.Add(-f.observedAge[id])
		stamped[id] = snap
	}
	return pace.RankWithStaleness(f.cfg, stamped, ids, modelID, at)
}

// rows is one status row per fixture credential, in the id order
// runtime.Status sorts by. A credential with no reading carries an empty
// window list rather than a nil one, which is what the host-backed status
// substitutes and what the page renders against.
func (f *fixture) rows(now time.Time, modelID string) []model.AuthStatus {
	shapeIdx := f.paceShapeIdx(now)
	auths := make([]model.AuthStatus, 0, len(f.auths))
	for _, a := range f.auths {
		snap, ok := f.snapshots[a.AuthID]
		snap.AuthID = a.AuthID
		if ok {
			snap.ObservedAt = now.Add(-f.observedAge[a.AuthID])
		} else {
			snap.Windows = []model.Window{}
		}
		a.Snapshot = snap
		a.Score = pace.ScoreWithStaleness(f.cfg, snap, ok, a.AuthID, modelID, now)
		a.History = []model.WindowHistory{}
		if hs, replayed := f.replayed[a.AuthID]; replayed {
			a.History = hs
		} else if ok {
			a.History = f.history(snap, now, shapeIdx)
		}
		auths = append(auths, a)
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].AuthID < auths[j].AuthID })
	return auths
}

// Status evaluates every fixture credential for modelID at now.
func (f *fixture) Status(now time.Time, modelID string) model.Status {
	now = now.Add(f.shift)
	if modelID == "" {
		modelID = modelFable
	}
	auths := f.rows(now, modelID)
	polled, next := f.pollTimes(now)
	return model.Status{
		Now:        now,
		Plugin:     f.plugin,
		Config:     f.cfg,
		Model:      modelID,
		PolledAt:   polled,
		NextPollAt: next,
		Auths:      auths,
		Bindings:   f.bindings,
		Decisions:  f.decisions,
		Warnings:   f.warnings,
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
			note: "every reading was older than max-staleness"},
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
			d.Scores = f.rankAt(snaps, ids, s.modelID, at)
		}
		out = append(out, d)
	}
	return out
}

// exhausted puts both seats past their 5-hour window and moves the log onto
// the overflow tier: the state where no seat can take a new conversation while
// every request is still being answered.
func (f *fixture) exhausted() {
	resets := map[string]time.Duration{
		seatAID: 4*time.Hour + 28*time.Minute,
		seatBID: 1*time.Hour + 6*time.Minute,
	}
	for id, in := range resets {
		snap := f.snapshots[id]
		snap.Source = model.SourceUsageEndpoint
		snap.Err, snap.ErrCategory = "", ""
		snap.Windows = append([]model.Window(nil), snap.Windows...)
		for i := range snap.Windows {
			w := &snap.Windows[i]
			if w.Kind != model.WindowSession {
				continue
			}
			w.Utilization, w.ResetsAt = 1.00, f.anchor.Add(in)
			w.Status, w.Severity, w.Active = model.StatusRejected, model.SeverityCritical, true
		}
		f.snapshots[id] = snap
	}
	f.observedAge[seatBID] = 51 * time.Second
	f.decisions = f.exhaustedDecisions()
}

// exhaustedDecisions scripts the log the exhausted pool produces: a run of
// requests on the overflow key, and behind it the seats holding their bindings
// while the host still offered them.
func (f *fixture) exhaustedDecisions() []model.Decision {
	keys := []string{"5f2c0b7d4a19e83c", "a91d33e0c7b45f28", "c4408b1ef6d92a70", "77b0fe4c1a8d6392"}
	models := []string{modelFable, modelOpus, modelSonnet}
	out := make([]model.Decision, 0, 30)
	for i := 0; i < 22; i++ {
		d := model.Decision{
			At:           f.anchor.Add(-time.Duration(8+17*i) * time.Second),
			SessionKey:   keys[i%len(keys)],
			Model:        models[i%len(models)],
			Provider:     "claude",
			ChosenAuthID: overflowKeyID,
			Kind:         model.DecisionAffinityHit,
			Subagent:     i%7 == 3,
		}
		if i%9 == 4 {
			// The host offers the overflow key alone, so it is the only
			// candidate a cold pick has to score.
			d.Kind = model.DecisionColdPick
			d.Note = "no eligible candidate; least-bound fallback"
			d.Scores = []model.Score{{AuthID: overflowKeyID, Reason: model.ReasonNoSnapshot}}
		}
		out = append(out, d)
	}
	ids := []string{seatAID, seatBID}
	for i, seat := range []string{seatAID, seatBID, seatAID} {
		at := f.anchor.Add(-time.Duration(9+4*i) * time.Minute)
		out = append(out, model.Decision{
			At: at, SessionKey: keys[i], Model: modelFable, Provider: "claude",
			ChosenAuthID: seat, Kind: model.DecisionAffinityHit,
			Note:   "binding kept; every seat is refused for this model",
			Scores: f.rankAt(f.snapshots, ids, modelFable, at),
		})
	}
	return out
}

// manySeat is one synthesized credential of the many scenario. The identity
// fields are the shapes a real pool mixes: two files of one account, two
// accounts whose masked addresses coincide, a host label, and files an
// operator named after a team.
type manySeat struct {
	id, label, name, email  string
	session, weekly, scoped float64
	// elapsedDays is how far into its weekly cycle the seat sits, which is
	// where its mark lands on the pace plot's x axis. Zero spreads the seat
	// across the axis by index instead.
	elapsedDays float64
	// state picks the exceptional condition the seat carries, "" for none.
	state string
}

// The first three rows are the pool `many -seats 3` renders, and they are the
// pool the README's hero shows: three plainly named seats two and a half, four
// and a half and six and a half days into their own weeks, so their marks
// spread across the pace plot, with the first a little under the pace target,
// the second a little over it and the third spent. Fable is most of what these
// seats spend, so each one's family cap sits within a quarter of its weekly
// figure — the critical seat excepted, whose cap over its week is the state it
// exists to draw. Behind them come the exceptional states, one per row, then
// the plain seats and the naming collisions a grown pool produces, so `many
// -seats 12` still draws every state the page can render.
var manySeats = []manySeat{
	{id: "claude-seat-1.json", label: "seat-1", name: "claude-seat-1.json", session: 0.31, weekly: 0.35, scoped: 0.28, elapsedDays: 2.5},
	{id: "claude-seat-2.json", label: "seat-2", name: "claude-seat-2.json", session: 0.58, weekly: 0.76, scoped: 0.62, elapsedDays: 4.5},
	{id: "claude-seat-3.json", label: "seat-3", name: "claude-seat-3.json", session: 0.45, weekly: 1.0, scoped: 1.0, elapsedDays: 6.5, state: "spent"},
	{id: "claude-ops@acme.example.json", name: "claude-ops@acme.example.json", email: "ops@acme.example", session: 0.12, weekly: 0.60, scoped: 0.90, elapsedDays: 3.5, state: "critical"},
	{id: "claude-quota.bot@acme-corp.example.json", name: "claude-quota.bot@acme-corp.example.json", email: "quota.bot@acme-corp.example", session: 0.05, weekly: 0.09, scoped: 0.0, state: "stale"},
	{id: "claude-team-data.json", name: "claude-team-data.json", email: "svc.data@acme.example", session: 0.52, weekly: 0.40, scoped: 0.33, state: "error"},
	{id: "claude-team-infra.json", name: "claude-team-infra.json", email: "svc.infra@acme.example", session: 0.0, weekly: 0.0, scoped: 0.0, state: "nosnap"},
	{id: "claude-team-mobile.json", name: "claude-team-mobile.json", email: "svc.mobile@acme.example", session: 0.67, weekly: 0.47, scoped: 0.51, state: "scoped-rejected"},
	{id: "claude-team-web.json", name: "claude-team-web.json", email: "svc.web@acme.example", session: 0.20, weekly: 0.15, scoped: 0.09, state: "disabled"},
	{id: "claude-oncall@acme.example.json", name: "claude-oncall@acme.example.json", email: "oncall@acme.example", session: 1.00, weekly: 0.58, scoped: 0.44, state: "rejected"},
	{id: "claude-alice-team-a.json", name: "claude-alice-team-a.json", email: "alice@example.com", session: 0.31, weekly: 0.055, scoped: 0.044, elapsedDays: 0.5},
	{id: "claude-alice-team-b.json", name: "claude-alice-team-b.json", email: "alice@example.com", session: 0.88, weekly: 0.27, scoped: 0.25, elapsedDays: 2},
	{id: "claude-team-ml.json", name: "claude-team-ml.json", email: "svc.ml@acme.example", session: 0.74, weekly: 0.66, scoped: 0.52, elapsedDays: 5},
	{id: "claude-team-qa.json", name: "claude-team-qa.json", email: "svc.qa@acme.example", session: 0.38, weekly: 0.29, scoped: 0.24},
}

// collide rewrites the two fixture seats so their pace-curve points land
// close together: seat A 3.5 days into its week at 48% of the weekly budget
// and 44% of its Fable cap, seat B 3.9 days in at 54.28% and, on the Fable
// cap, spent. The two weekly gaps differ by under a tenth of a point, so the
// Standard table prints one cost for both seats and ranks them apart.
func (f *fixture) collide() {
	f.observedAge[seatBID] = 40 * time.Second
	set := func(id string, weekly, scoped, elapsedDays float64) {
		snap := f.snapshots[id]
		snap.Source = model.SourceUsageEndpoint
		snap.Err, snap.ErrCategory = "", ""
		snap.Windows = append([]model.Window(nil), snap.Windows...)
		resets := f.anchor.Add(time.Duration((7 - elapsedDays) * float64(24*time.Hour)))
		for i := range snap.Windows {
			w := &snap.Windows[i]
			switch w.Kind {
			case model.WindowSession:
				w.Utilization, w.Status, w.Severity = 0.35, model.StatusAllowed, model.SeverityNormal
			case model.WindowWeekly:
				w.Utilization, w.ResetsAt, w.Status, w.Severity = weekly, resets, model.StatusAllowed, model.SeverityNormal
			case model.WindowWeeklyScoped:
				w.Utilization, w.ResetsAt = scoped, resets
				w.Status, w.Severity = model.StatusAllowed, model.SeverityNormal
			}
		}
		f.snapshots[id] = snap
		f.snapshotsPast[id] = withSessionUtil(snap, 0.2, model.StatusAllowed, model.SeverityNormal)
	}
	set(seatAID, 0.48, 0.44, 3.5)
	set(seatBID, 0.5428, 1.0, 3.9)
	f.decisions = f.buildDecisions()
}

// unreadAfterClear is how long the cleared scenario leaves a seat's
// all-models window unread after its clearing.
const unreadAfterClear = 6 * time.Hour

func (f *fixture) cleared() {
	snap := f.snapshots[seatBID]
	snap.Windows = append([]model.Window(nil), snap.Windows...)
	resets := f.anchor.Add(3 * 24 * time.Hour)
	for i := range snap.Windows {
		w := &snap.Windows[i]
		switch w.Kind {
		case model.WindowWeekly:
			w.Utilization, w.ResetsAt, w.Status, w.Severity = 0.19, resets, model.StatusAllowed, model.SeverityNormal
		case model.WindowWeeklyScoped:
			w.Utilization, w.ResetsAt = 0.18, resets
		}
	}
	f.snapshots[seatBID] = snap
	f.clearedAgo = map[string]time.Duration{seatBID: 12 * time.Hour}
}

// growTo replaces the fixture's credentials with n synthesized seats. Past the
// table above, seats repeat its rows under numbered names.
func (f *fixture) growTo(n int) {
	f.auths = nil
	f.snapshots = map[string]model.AuthSnapshot{}
	f.snapshotsPast = map[string]model.AuthSnapshot{}
	f.observedAge = map[string]time.Duration{}
	f.bindings = nil
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ms := manySeats[i%len(manySeats)]
		if i >= len(manySeats) {
			suffix := fmt.Sprintf("-%d", i/len(manySeats)+1)
			ms.id = strings.TrimSuffix(ms.id, ".json") + suffix + ".json"
			ms.name = ms.id
			if ms.label != "" {
				ms.label += suffix
			}
		}
		f.addManySeat(i, ms)
		ids = append(ids, ms.id)
	}
	f.decisions = f.manyDecisions(ids)
}

func (f *fixture) addManySeat(i int, ms manySeat) {
	label := ms.label
	if label == "" {
		label = ms.email
	}
	if label == "" {
		label = ms.name
	}
	a := model.AuthStatus{
		AuthID: ms.id, Label: label, Name: ms.name, Email: ms.email,
		Provider: "claude", Priority: 10, HostStatus: "active", Bindings: 1 + i%3,
		Cache: model.CacheStats{
			Requests:            int64(120 + 90*i),
			CacheReadTokens:     int64(800_000 + 400_000*i),
			CacheCreationTokens: int64(60_000 + 20_000*i),
			FreshInputTokens:    int64(30_000 + 25_000*(i%5)*i),
			OutputTokens:        int64(40_000 + 9_000*i),
		},
	}
	f.observedAge[ms.id] = time.Duration(20+13*i) * time.Second
	elapsed := ms.elapsedDays
	if elapsed <= 0 {
		// Seats carrying no elapsedDays take axis positions the scripted ones
		// leave open: a quarter-day lattice offset from theirs, which repeats
		// only after 25 seats and never lands on one of their marks.
		elapsed = 0.875 + math.Mod(1+1.5*float64(i), 6.25)
	}
	// Both weekly windows reset together, as a provider resets a seat's whole
	// week at one instant.
	weeklyReset := f.anchor.Add(time.Duration((7 - elapsed) * float64(24*time.Hour)))
	snap := model.AuthSnapshot{
		AuthID: ms.id, Label: label, Source: model.SourceUsageEndpoint,
		Windows: []model.Window{
			{
				Kind: model.WindowSession, Utilization: ms.session,
				ResetsAt: f.anchor.Add(time.Duration(40+25*i) * time.Minute),
				Duration: model.SessionDuration,
				Status:   model.StatusAllowed, Severity: model.SeverityNormal, Active: true,
			},
			{
				Kind: model.WindowWeekly, Utilization: ms.weekly,
				ResetsAt: weeklyReset,
				Duration: model.WeeklyDuration,
				Status:   model.StatusAllowed, Severity: model.SeverityNormal,
			},
			{
				Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: ms.scoped,
				ResetsAt: weeklyReset,
				Duration: model.WeeklyDuration,
				Status:   model.StatusAllowed, Severity: model.SeverityNormal,
			},
		},
	}
	switch ms.state {
	case "rejected":
		snap.Windows[0].Status = model.StatusRejected
		snap.Windows[0].Severity = model.SeverityCritical
	case "critical":
		// Critical headroom on the family cap with nothing rejected: the seat
		// is still serving every request and is still eligible.
		snap.Windows[2].Status = model.StatusAllowedWarning
		snap.Windows[2].Severity = model.SeverityCritical
	case "scoped-rejected":
		// The provider refused the family cap and nothing else: the seat still
		// takes Standard requests and is out for Fable ones, which is the state
		// that puts a verdict per family in the seat's header row.
		snap.Windows[2].Status = model.StatusRejected
		snap.Windows[2].Severity = model.SeverityCritical
	case "spent":
		snap.Windows[1].Status = model.StatusAllowedWarning
		snap.Windows[1].Severity = model.SeverityWarning
	case "stale":
		f.observedAge[ms.id] = f.cfg.Quota.MaxStaleness + 5*time.Minute
	case "error":
		snap.Source = model.SourceResponseHeaders
		snap.Err = "Get \"https://api.anthropic.com/api/oauth/usage\": context deadline exceeded"
		snap.ErrCategory = "timeout"
	case "disabled":
		// The poller skips a credential the host has disabled, so it never
		// holds a reading.
		a.HostStatus = "disabled"
	}
	if ms.state != "nosnap" && ms.state != "disabled" {
		f.snapshots[ms.id] = snap
	}
	f.auths = append(f.auths, a)
	for b := 0; b < a.Bindings; b++ {
		key := fmt.Sprintf("%02x%02x0b7d4a19e83c", i, b)
		f.bindings = append(f.bindings, model.Binding{
			SessionKey: key, Provider: "claude", Model: []string{modelFable, modelOpus, modelSonnet}[(i+b)%3],
			AuthID: ms.id, BoundAt: f.anchor.Add(-time.Duration(9+31*i+7*b) * time.Minute),
			LastSeen: f.anchor.Add(-time.Duration(5+17*i+3*b) * time.Second), Hits: 3 + 41*b + 7*i,
		})
	}
}

// manyDecisions scripts a log in which every seat with a binding appears, with
// a cold pick scored against the whole pool every few rows.
func (f *fixture) manyDecisions(ids []string) []model.Decision {
	kinds := []string{model.DecisionAffinityHit, model.DecisionAffinityHit, model.DecisionColdPick,
		model.DecisionAffinityHit, model.DecisionFailover, model.DecisionAffinityHit, model.DecisionDeclined}
	out := make([]model.Decision, 0, 48)
	var bound []model.Binding
	for i := 0; i < 48; i++ {
		at := f.anchor.Add(-time.Duration(20+70*i) * time.Second)
		kind := kinds[i%len(kinds)]
		d := model.Decision{At: at, Provider: "claude", Kind: kind}
		// A decision that lands on a binding reports the binding's model; one
		// that does not reports what the request asked for.
		requested := modelFable
		if i%4 == 1 {
			requested = modelOpus
		}
		switch kind {
		case model.DecisionDeclined:
			d.Model = requested
			d.Note = "every reading was older than max-staleness"
		case model.DecisionColdPick:
			d.Model = requested
			d.Scores = f.rankAt(f.snapshots, ids, d.Model, at)
			d.SessionKey = fmt.Sprintf("%08x9a35b18e", 0x6c2a90f3+i)
			if len(d.Scores) > 0 && d.Scores[0].Eligible {
				d.ChosenAuthID = d.Scores[0].AuthID
			}
		default:
			if len(bound) == 0 {
				bound = f.bindings
			}
			b := bound[i%len(bound)]
			d.SessionKey = b.SessionKey
			d.Model = b.Model
			d.ChosenAuthID = b.AuthID
			d.Subagent = i%5 == 3
			if kind == model.DecisionFailover {
				d.PreviousAuthID = ids[(i+1)%len(ids)]
				d.Note = "bound credential was not among the candidates the host offered"
				d.Scores = f.rankAt(f.snapshots, ids, d.Model, at)
			}
		}
		out = append(out, d)
	}
	return out
}

// history synthesizes the utilization record the poller would have built for
// a snapshot, through a real quota store so it is thinned the way the plugin
// thins. The seat's id seeds every choice, so a pool draws a different line
// per seat and a reload draws the same ones.
//
// The two weekly windows replay a real week from pace_shapes.json, indexed by
// wall-clock time: the recording's 7-day span ends at now, so seats read the
// same recorded position at the same instant however far apart their cycles
// started, and paceCurve stretches the replay onto the reading the snapshot
// carries. One recording serves the whole pool and both weekly windows of one
// seat, so every seat's flats and bursts fall at the same wall-clock times and
// a seat's family cap moves with its week. The 5-hour window keeps a smooth power
// curve, which is what a window that short and steep looks like; a third of
// the seats get a steeper last forty minutes, the case the chart's
// conversation markers exist for, and another third hold it flat through the
// middle half of the cycle, so the store folds that stretch to two readings
// hours apart and the chart's step between them is visible.
//
// The completed cycle is an estimate reconstructed from a token log, as a
// backfilled file holds it; a weekly one replays the other recording over the
// span ending at the cycle's start, at an hour a step, because a log is
// coarser than a poll. Seats with seed%3 == 1
// also start the current cycle as an estimate over its first 45%, so the chart
// shows a mixed cycle: estimated, then observed from the plugin's first
// reading.
func (f *fixture) history(snap model.AuthSnapshot, now time.Time, shapeIdx int) []model.WindowHistory {
	seed := 0
	for _, c := range snap.AuthID {
		seed = (seed*31 + int(c)) % 997
	}
	sessionShape := 0.8 + float64(seed%9)/10
	store := quota.NewStore()
	var saved []model.WindowHistory
	for _, w := range snap.Windows {
		if w.Duration <= 0 || w.ResetsAt.IsZero() {
			continue
		}
		step := 2 * time.Minute
		if w.Duration > 24*time.Hour {
			step = 20 * time.Minute
		}
		walks := w.Kind != model.WindowSession
		start := w.ResetsAt.Add(-w.Duration)
		prevLanding := 0.55 + float64(seed%46)/100
		if w.Kind == model.WindowSession {
			prevLanding = 0.3 + float64(seed%60)/100
		}
		var prev, cur model.Cycle
		prev.ResetsAt, prev.Estimated = start, true
		cur.ResetsAt = w.ResetsAt
		if walks {
			prevStep := time.Hour
			prevStart := start.Add(-w.Duration)
			other := paceShapes[(shapeIdx+1)%len(paceShapes)]
			for i, u := range paceCurve(other, 1, prevLanding, int(w.Duration/prevStep)+1) {
				prev.Samples = append(prev.Samples, model.Sample{At: prevStart.Add(time.Duration(i) * prevStep), Utilization: u})
			}
		} else {
			for t := start.Add(-w.Duration); t.Before(start); t = t.Add(step) {
				e := 1 - start.Sub(t).Seconds()/w.Duration.Seconds()
				prev.Samples = append(prev.Samples, model.Sample{At: t, Utilization: prevLanding * math.Pow(e, sessionShape)})
			}
		}
		span := now.Sub(start).Seconds()
		steep := !walks && seed%3 == 0
		idle := !walks && seed%3 == 2
		knee := 1 - (40*time.Minute).Seconds()/span
		mixedUntil := start.Add(-time.Second)
		if seed%3 == 1 {
			mixedUntil = start.Add(time.Duration(0.45 * float64(now.Sub(start))))
		}
		put := func(at time.Time, util float64) {
			r := w
			r.Utilization = util
			store.Put(model.AuthSnapshot{AuthID: snap.AuthID, ObservedAt: at, Source: model.SourceUsageEndpoint, Windows: []model.Window{r}})
		}
		n := int(now.Sub(start)/step) + 1
		var curve []float64
		if walks {
			elapsed := span / w.Duration.Seconds()
			curve = paceCurve(paceShapes[shapeIdx], elapsed, w.Utilization, n)
		}
		// The sample the provider cleared the window after, or -1. The replay
		// is stretched to near full there, and the fresh window climbs
		// steadily from zero to the reading. The all-models window goes
		// unread for unreadAfterClear after the clearing, the way a host that
		// was down across it records the window; the cap is read throughout.
		ci, unread := -1, 0
		if ago, ok := f.clearedAgo[snap.AuthID]; ok && walks {
			ci = min(max(int(now.Add(-ago).Sub(start)/step), 0), n-1)
			if w.Kind == model.WindowWeekly {
				unread = int(unreadAfterClear / step)
			}
		}
		for i := 0; i < n; i++ {
			t := start.Add(time.Duration(i) * step)
			frac := t.Sub(start).Seconds() / span
			u := w.Utilization * math.Pow(frac, sessionShape)
			if walks {
				u = curve[i]
			}
			if ci >= 0 && i <= ci {
				u = math.Min(1, 0.98*curve[i]/math.Max(curve[ci], 1e-9))
			} else if ci >= 0 {
				u = w.Utilization * float64(i-ci) / float64(n-1-ci)
			}
			if idle && frac > 0.25 && frac < 0.75 {
				u = w.Utilization * math.Pow(0.25, sessionShape)
			}
			if steep && knee > 0 {
				if frac < knee {
					u = w.Utilization * 0.35 * frac / knee
				} else {
					u = w.Utilization * (0.35 + 0.65*(frac-knee)/(1-knee))
				}
			}
			if !t.After(mixedUntil) {
				cur.Samples = append(cur.Samples, model.Sample{At: t, Utilization: u})
				continue
			}
			if len(cur.Samples) > 0 {
				cur.Estimated = true
			}
			if ci >= 0 && i > ci && i <= ci+unread {
				continue
			}
			put(t, u)
		}
		h := model.WindowHistory{Kind: w.Kind, Scope: w.Scope, Cycles: []model.Cycle{prev}}
		if len(cur.Samples) > 0 {
			h.Cycles = append(h.Cycles, cur)
		}
		saved = append(saved, h)
	}
	store.ImportHistory(map[string][]model.WindowHistory{snap.AuthID: saved}, now)
	return store.History(snap.AuthID, quota.HistoryPublishMax, now)
}

//go:embed pace_shapes.json
var paceShapesJSON []byte

// pacePoint is one reading of a recorded week: frac is how far through the
// week it landed, util the utilization the provider reported then.
type pacePoint struct{ frac, util float64 }

// paceShapes are two real completed 7-day cycles, ordered by frac and carrying
// nothing that identifies the seats they came from. Every weekly curve the
// harness draws is one of them stretched, which is what gives the fixture the
// flats, the bursts and the line-crossings a synthesized walk has to guess at.
var paceShapes = loadPaceShapes()

func loadPaceShapes() [][]pacePoint {
	var raw [][][2]float64
	if err := json.Unmarshal(paceShapesJSON, &raw); err != nil {
		panic("webdev: decode pace_shapes.json: " + err.Error())
	}
	out := make([][]pacePoint, 0, len(raw))
	for _, cycle := range raw {
		pts := make([]pacePoint, len(cycle))
		for i, p := range cycle {
			pts[i] = pacePoint{frac: p[0], util: p[1]}
		}
		out = append(out, pts)
	}
	return out
}

// paceReading is the recorded reading in force at frac, which holds each flat
// flat: a sample between two readings carries the earlier one rather than a
// value the provider never reported. Before the first reading a cycle is at
// zero.
// paceFit is one seat's weekly window as the recording has to fit it: how far
// through its cycle the seat sits, and the utilization the replay has to land
// on.
type paceFit struct{ elapsed, end float64 }

// paceShapeFor is the index of the recording whose replay, landed on each
// seat's endpoint, sits nearest the pace target summed over the whole pool.
// One recording serves every seat, which is what puts the same usage pattern
// at the same wall-clock instant on all of them; a pool that ran over its
// curve draws the week that ran over, one that ran under draws the week that
// ran under.
func paceShapeFor(cfg model.PaceConfig, seats []paceFit) int {
	const n = 200
	best, bestDist := 0, math.Inf(1)
	for i, shape := range paceShapes {
		dist := 0.0
		for _, s := range seats {
			for k, u := range paceCurve(shape, s.elapsed, s.end, n) {
				dist += math.Abs(u - pace.Target(cfg, s.elapsed*float64(k)/float64(n-1)))
			}
		}
		if dist < bestDist {
			best, bestDist = i, dist
		}
	}
	return best
}

// paceShapeIdx is the recording the pool replays, fitted to every seat holding
// a weekly reading.
func (f *fixture) paceShapeIdx(now time.Time) int {
	var seats []paceFit
	for _, snap := range f.snapshots {
		for _, w := range snap.Windows {
			if w.Kind == model.WindowWeekly && w.Duration > 0 && !w.ResetsAt.IsZero() {
				start := w.ResetsAt.Add(-w.Duration)
				seats = append(seats, paceFit{elapsed: now.Sub(start).Seconds() / w.Duration.Seconds(), end: w.Utilization})
				break
			}
		}
	}
	return paceShapeFor(f.cfg.Pace, seats)
}

func paceReading(shape []pacePoint, frac float64) pacePoint {
	i := sort.Search(len(shape), func(i int) bool { return shape[i].frac > frac })
	if i == 0 {
		return pacePoint{}
	}
	return shape[i-1]
}

// paceCurve lays a recorded week over the stretch this seat has lived through
// and returns n samples of utilization, 0 at the cycle's start and end at its
// last sample. The recording is indexed by wall-clock time: its 7-day span
// ends where the seat's stretch ends, so a sample a fraction d of a week
// before that end reads the recording at 1-d, and a seat 2.5 days into its
// week replays the recording's last 2.5 days. Seats whose cycles started days
// apart therefore read the same recorded position at the same instant, and
// the flat one of them draws over a night is the flat all of them draw. The
// replay is the rises the recording shows: each step adds what the recording
// added, so the curve carries that week's overnight flats, its bursts and the
// rate at which it tracked the pace target. The rises are scaled by one factor
// so their sum is the reading the snapshot carries; nothing is added between
// them, so a step the recording spent nothing on stays exactly flat and an
// idle night is one flat on the chart, not a slope of twenty-minute increments.
//
// Utilization is read from the recording rather than interpolated between its
// readings, so the staircase the poller recorded survives the resampling and an
// idle run stays one flat. A cycle that ends spent reaches full before its
// reset and holds there, so the recording is laid over the first 86% of the
// elapsed span and the rest of the curve is the plateau.
func paceCurve(shape []pacePoint, elapsed, end float64, n int) []float64 {
	if n < 1 {
		return nil
	}
	out := make([]float64, n)
	out[n-1] = end
	if n < 2 || len(shape) == 0 || elapsed <= 0 || end <= 0 {
		return out
	}
	hold := elapsed
	if end >= 1 {
		hold = 0.86 * elapsed
	}
	// The recording ends where the seat's stretch does, so the replay starts
	// as far back in the recording as the seat is into its cycle.
	offset := 1 - elapsed
	prev := paceReading(shape, offset).util
	rises := make([]float64, n)
	replayed := 0.0
	for i := 1; i < n; i++ {
		f := math.Min(elapsed*float64(i)/float64(n-1), hold)
		cur := paceReading(shape, offset+f).util
		if d := cur - prev; d > 0 {
			rises[i] = d
			replayed += d
		}
		prev = cur
	}
	// The gap between what the recording rose and where the seat ends is
	// closed inside the rises, each scaled by one factor, and never spread
	// over time: a step that recorded no spend stays exactly flat, so an
	// idle night stays a flat on the chart rather than becoming a slope of
	// twenty-minute increments.
	gain := 1.0
	if replayed > 0 {
		gain = end / replayed
	}
	for i := 1; i < n; i++ {
		out[i] = math.Min(out[i-1]+rises[i]*gain, 1)
	}
	// Utilization never falls inside a cycle, and the last sample is the
	// reading the snapshot carries, so the rise is clamped from both ends.
	out[0] = 0
	for i := 1; i < n; i++ {
		out[i] = math.Max(out[i], out[i-1])
	}
	out[n-1] = end
	for i := n - 2; i > 0; i-- {
		out[i] = math.Min(out[i], out[i+1])
	}
	return out
}
