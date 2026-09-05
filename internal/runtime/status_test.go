package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
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
	if status.Plugin.Name != "cpa-claude-quota-scheduler" || status.Plugin.Version == "" || status.Plugin.HostSchemaVersion != 4 || !status.Plugin.StartedAt.Equal(testNow) {
		t.Errorf("Plugin = %+v", status.Plugin)
	}
	if !status.Config.Enabled || status.Config.Pace.HardCutoff != 0.98 {
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
