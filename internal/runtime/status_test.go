package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func TestStatusFillsEveryField(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.registerManagement(t)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	tp.pick(t, pickRequest(fableModel, "k1", "claude-a.json", "claude-b.json"))
	tp.pick(t, pickRequest(fableModel, "k2", "claude-a.json", "claude-b.json"))
	tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
		AuthID: "claude-a.json", Model: fableModel,
		Detail: UsageDetail{InputTokens: 10, CacheReadTokens: 900, CacheCreationTokens: 90},
	}), nil)

	later := testNow.Add(time.Minute)
	status := tp.Status(later, "")

	if !status.Now.Equal(later) {
		t.Errorf("Now = %v", status.Now)
	}
	if status.Plugin.Name != "claude-seat-pacer" || status.Plugin.Version == "" || status.Plugin.HostSchemaVersion != 4 || !status.Plugin.StartedAt.Equal(testNow) {
		t.Errorf("Plugin = %+v", status.Plugin)
	}
	if !status.Config.Enabled || status.Config.Pace.LandingTarget != 1.10 {
		t.Errorf("Config = %+v, want the live config", status.Config)
	}
	if status.Model != fableModel {
		t.Errorf("Model = %q, want the most recently routed model", status.Model)
	}
	if len(status.Auths) != 4 {
		t.Fatalf("Auths = %d rows", len(status.Auths))
	}
	rows := make(map[string]model.AuthStatus)
	for _, row := range status.Auths {
		rows[row.AuthID] = row
	}
	a := rows["claude-a.json"]
	if a.Label != "a@example.com" || a.Provider != "claude" || a.Priority != 3 || a.HostStatus != "active" {
		t.Errorf("row a = label %q provider %q priority %d status %q", a.Label, a.Provider, a.Priority, a.HostStatus)
	}
	if a.Name != "claude-a.json" || a.Email != "a@example.com" {
		t.Errorf("row a = name %q email %q, want the host's file name and address", a.Name, a.Email)
	}
	if len(a.Snapshot.Windows) == 0 || a.Snapshot.Source != model.SourceUsageEndpoint {
		t.Errorf("row a snapshot = %+v", a.Snapshot)
	}
	if !a.Score.Eligible || len(a.Score.Windows) == 0 {
		t.Errorf("row a score = %+v, want an eligible pace evaluation", a.Score)
	}
	if a.Cache.Requests != 1 || a.Cache.CacheReadTokens != 900 {
		t.Errorf("row a cache = %+v", a.Cache)
	}
	if rows["claude-a.json"].Bindings+rows["claude-b.json"].Bindings != 2 {
		t.Errorf("bindings per row = a %d b %d, want two in total", rows["claude-a.json"].Bindings, rows["claude-b.json"].Bindings)
	}
	if off := rows["claude-off.json"]; off.HostStatus != "disabled" || off.Score.Reason != model.ReasonNoSnapshot {
		t.Errorf("disabled row = %+v", off)
	}
	if len(status.Bindings) != 2 {
		t.Errorf("Bindings = %+v", status.Bindings)
	}
	if len(status.Decisions) != 2 || !status.Decisions[0].At.Equal(testNow) || status.Decisions[0].SessionKey != "k2" {
		t.Errorf("Decisions = %+v, want two, newest first", status.Decisions)
	}
	if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "claude-key") {
		t.Errorf("Warnings = %v", status.Warnings)
	}

	// An explicit model overrides the default, and the whole thing encodes.
	if got := tp.Status(later, "claude-opus-5").Model; got != "claude-opus-5" {
		t.Errorf("Model override = %q", got)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET", "refresh_token", "rt-a", "sk-ant"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("status JSON leaks %q", secret)
		}
	}
	for _, key := range []string{`"warnings":[`, `"bindings":[`, `"decisions":[`, `"auths":[`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("status JSON lacks %s as an array", key)
		}
	}
}

func TestStatusBeforeAnyTrafficIsComplete(t *testing.T) {
	tp := newTestPlugin(t, "enabled: false\n")
	status := tp.Status(testNow, "")
	if status.Model != defaultStatusModel {
		t.Errorf("Model = %q, want %q", status.Model, defaultStatusModel)
	}
	if status.Auths == nil || status.Bindings == nil || status.Decisions == nil || status.Warnings == nil {
		t.Errorf("nil slices in %+v", status)
	}
	if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "disabled") {
		t.Errorf("Warnings = %v, want the disabled notice", status.Warnings)
	}
}

func TestStatusMarksStaleSnapshots(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	a := seatA(t)
	a.ObservedAt = testNow.Add(-time.Hour)
	tp.quota.Put(a)
	status := tp.Status(testNow, fableModel)
	if len(status.Auths) != 1 || status.Auths[0].Score.Reason != model.ReasonStale {
		t.Errorf("Auths = %+v, want seat-a scored stale", status.Auths)
	}
}

func TestDecisionLogRingAndResize(t *testing.T) {
	l := newDecisionLog(3)
	for i := 1; i <= 5; i++ {
		l.add(model.Decision{Note: string(rune('0' + i))})
	}
	got := l.newestFirst()
	if len(got) != 3 || got[0].Note != "5" || got[2].Note != "3" {
		t.Errorf("newestFirst = %+v", got)
	}
	l.resize(2)
	if got := l.newestFirst(); len(got) != 2 || got[0].Note != "5" || got[1].Note != "4" {
		t.Errorf("after shrink = %+v", got)
	}
	l.resize(4)
	l.add(model.Decision{Note: "6"})
	if got := l.newestFirst(); len(got) != 3 || got[0].Note != "6" || got[2].Note != "4" {
		t.Errorf("after grow = %+v", got)
	}
}

func TestStatusPublishesUtilizationHistory(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	tp.now = func() time.Time { return testNow.Add(2 * time.Minute) }
	tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
		AuthID: "claude-a.json", Model: fableModel, RequestedAt: testNow.Add(2 * time.Minute),
		ResponseHeaders: map[string][]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(testNow.Add(3*time.Hour).Unix(), 10)},
		},
	}), nil)

	status := tp.Status(testNow.Add(3*time.Minute), "")
	for _, row := range status.Auths {
		if row.History == nil {
			t.Errorf("row %s has nil history; the page expects an array", row.AuthID)
		}
		if row.AuthID != "claude-a.json" {
			continue
		}
		var session *model.WindowHistory
		for i := range row.History {
			if row.History[i].Kind == model.WindowSession {
				session = &row.History[i]
			}
		}
		if session == nil || session.Samples() != 2 {
			t.Fatalf("session history = %+v, want the poll and the header reading", row.History)
		}
		if got := session.Cycles[len(session.Cycles)-1].Samples[1].Utilization; got != 0.42 {
			t.Errorf("newest sample = %v, want the header's 0.42", got)
		}
	}
}

func TestHistoryFileIsWrittenAfterAPollAndReadAtStart(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(tp.opts.HistoryFile)
	if err != nil {
		t.Fatalf("history file after a poll: %v", err)
	}
	if !strings.Contains(string(body), `"claude-a.json"`) || strings.Contains(string(body), secretToken) {
		t.Errorf("history file = %s", body)
	}

	// A second plugin on the same file sees the first run's samples once its
	// own poll lands, behind the new reading.
	again := &testPlugin{host: tp.host}
	again.Plugin = New(Options{
		Name: "claude-seat-pacer", Version: "0.0.0-test", Author: "a", Repository: "https://example.invalid/repo",
		Host: tp.host.call, HistoryFile: tp.opts.HistoryFile,
		Now: func() time.Time { return testNow.Add(2 * time.Minute) },
	})
	again.startDelay = time.Hour
	again.fetchStagger = 0
	t.Cleanup(again.Shutdown)
	again.register(t, MethodPluginRegister, testConfigYAML)
	if err := again.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := again.Status(testNow.Add(3*time.Minute), "").Auths[0]
	if row.AuthID != "claude-a.json" || len(row.History) == 0 || row.History[0].Samples() != 2 {
		t.Errorf("history after reload = %+v, want the saved sample and the new one", row.History)
	}

	// Persistence off writes nothing.
	off := newTestPlugin(t, testConfigYAML+"quota:\n  persist-history: false\n")
	pollFixture(t, off)
	if err := off.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(off.opts.HistoryFile); !os.IsNotExist(err) {
		t.Errorf("history file exists with persistence off: %v", err)
	}
}

// seededPlugin registers a plugin whose history file already holds path's
// contents, so the load the poller runs at registration sees them.
func seededPlugin(t *testing.T, path, configYAML string) *testPlugin {
	t.Helper()
	tp := &testPlugin{host: newFakeHost()}
	tp.Plugin = New(Options{
		Name: "claude-seat-pacer", Version: "0.0.0-test", Author: "yuya-iwabuchi",
		Repository: "https://example.invalid/repo",
		Host:       tp.host.call, HistoryFile: path,
		Now: func() time.Time { return testNow },
	})
	tp.startDelay = time.Hour
	tp.fetchStagger = 0
	t.Cleanup(tp.Shutdown)
	pollFixture(t, tp)
	tp.register(t, MethodPluginRegister, configYAML)
	return tp
}

// TestHistoryFileSurvivesAPollThatNeverLoadedIt covers the two runs that must
// not write the file: one with persistence off, which never reads it, and one
// whose read failed. Either would otherwise replace a previous run's samples
// with only what it recorded itself.
func TestHistoryFileSurvivesAPollThatNeverLoadedIt(t *testing.T) {
	const saved = `{"version":1,"seats":{"claude-a.json":[{"kind":"five_hour","cycles":[{"resets_at":"2026-09-04T21:00:00Z","samples":[[1757001600,0.5]]}]}]}}`

	// Persistence off leaves the file exactly as it was, and persistence
	// turned on by a reload loads it before the next poll saves.
	path := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(path, []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}
	off := seededPlugin(t, path, testConfigYAML+"quota:\n  persist-history: false\n")
	if err := off.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != saved {
		t.Fatalf("history file with persistence off = %s (%v), want it untouched", body, err)
	}

	off.register(t, MethodPluginReconfigure, testConfigYAML)
	if err := off.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := off.Status(testNow, "").Auths[0]
	if row.AuthID != "claude-a.json" || len(row.History) == 0 || row.History[0].Samples() != 2 {
		t.Errorf("history after persistence was turned on = %+v, want the saved sample and the new one", row.History)
	}

	// A read that failed leaves the file for the next run rather than
	// replacing it with this run's samples.
	const unreadable = `{"version":99,"seats":{}}`
	badPath := filepath.Join(t.TempDir(), "history.json")
	if err := os.WriteFile(badPath, []byte(unreadable), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := seededPlugin(t, badPath, testConfigYAML)
	if err := bad.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(badPath); err != nil || string(body) != unreadable {
		t.Fatalf("history file after a failed read = %s (%v), want it untouched", body, err)
	}
}
