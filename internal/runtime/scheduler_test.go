package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// seats installs the live-seat fixtures as fresh snapshots.
func (tp *testPlugin) seats(t *testing.T) {
	t.Helper()
	tp.quota.Put(seatA(t))
	tp.quota.Put(seatB(t))
}

func TestPickDeclinesForUngovernedProvider(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)

	req := pickRequest("gemini-3-pro", "k", "gem-a", "gem-b")
	req.Providers = []string{"gemini"}
	for i := range req.Candidates {
		req.Candidates[i].Provider = "gemini"
	}
	raw, ok := tp.Call(MethodSchedulerPick, mustJSON(t, req))
	if !ok {
		t.Fatalf("decline came back as an error envelope: %s", raw)
	}
	var out SchedulerPickResponse
	if err := unwrapEnvelope(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Handled || out.AuthID != "" {
		t.Errorf("response = %+v, want Handled false", out)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionDeclined || !strings.Contains(d.Note, "provider") {
		t.Errorf("decision = %+v, want a declined decision naming the provider", d)
	}
}

func TestPickDeclines(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		req     func(t *testing.T) SchedulerPickRequest
		snaps   bool
		noteHas string
	}{
		{
			name:  "plugin disabled",
			yaml:  "enabled: false\n",
			req:   func(*testing.T) SchedulerPickRequest { return pickRequest(fableModel, "k", "seat-a", "seat-b") },
			snaps: true, noteHas: "disabled",
		},
		{
			name:  "model not governed",
			yaml:  testConfigYAML + "models: [claude-opus-5]\n",
			req:   func(*testing.T) SchedulerPickRequest { return pickRequest(fableModel, "k", "seat-a", "seat-b") },
			snaps: true, noteHas: "model",
		},
		{
			name:    "zero candidates",
			yaml:    testConfigYAML,
			req:     func(*testing.T) SchedulerPickRequest { return pickRequest(fableModel, "k") },
			snaps:   true,
			noteHas: "candidates",
		},
		{
			name:    "no key and no snapshot",
			yaml:    testConfigYAML,
			req:     func(*testing.T) SchedulerPickRequest { return pickRequest(fableModel, "", "seat-a", "seat-b") },
			noteHas: "no session key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, tc.yaml)
			if tc.snaps {
				tp.seats(t)
			}
			resp := tp.pick(t, tc.req(t))
			if resp.Handled {
				t.Fatalf("handled with %q, want a decline", resp.AuthID)
			}
			d := tp.lastDecision(t)
			if d.Kind != model.DecisionDeclined || !strings.Contains(d.Note, tc.noteHas) {
				t.Errorf("decision = %+v, want declined with note containing %q", d, tc.noteHas)
			}
			if tp.bindings.Len() != 0 {
				t.Error("a decline created a binding")
			}
		})
	}
}

func TestPickNeverErrorsOnGarbage(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	raw, ok := tp.Call(MethodSchedulerPick, []byte("not json"))
	if !ok {
		t.Fatalf("garbage pick produced an error envelope: %s", raw)
	}
	var out SchedulerPickResponse
	if err := unwrapEnvelope(raw, &out); err != nil || out.Handled {
		t.Errorf("response = %+v err=%v, want Handled false", out, err)
	}
}

func TestColdPickChoosesPaceWinnerAndBinds(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	key := tp.bridgeKeyFor(t, claudeCodeBody("11111111-1111-1111-1111-111111111111"))

	resp := tp.pick(t, pickRequest(fableModel, key, "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want seat-b: its weekly window resets in hours", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionColdPick || d.ChosenAuthID != "seat-b" || d.SessionKey != key || d.Provider != "claude" || d.Model != fableModel {
		t.Errorf("decision = %+v", d)
	}
	if len(d.Scores) != 2 || !d.Scores[0].Eligible || d.Scores[0].AuthID != "seat-b" {
		t.Errorf("scores = %+v, want both seats with seat-b ranked first", d.Scores)
	}
	b, ok := tp.bindings.Lookup("claude", fableModel, key, testNow)
	if !ok || b.AuthID != "seat-b" {
		t.Errorf("binding = %+v ok=%v, want seat-b", b, ok)
	}
}

func TestColdPickWithoutKeyRoutesButDoesNotBind(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	resp := tp.pick(t, pickRequest(fableModel, "", "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want seat-b", resp)
	}
	if tp.bindings.Len() != 0 {
		t.Error("a keyless pick created a binding")
	}
	if d := tp.lastDecision(t); !strings.Contains(d.Note, "not pinned") {
		t.Errorf("note = %q, want it to say the session is not pinned", d.Note)
	}
}

func TestPickHonoursExistingBindingEvenWhenBehindPace(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	key := "k-bound"
	// seat-a loses the cold pick; the binding keeps it anyway.
	tp.bindings.Bind("claude", fableModel, key, "seat-a", testNow.Add(-time.Minute))

	resp := tp.pick(t, pickRequest(fableModel, key, "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want the bound seat-a", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionAffinityHit || d.ChosenAuthID != "seat-a" || d.PreviousAuthID != "" {
		t.Errorf("decision = %+v, want affinity-hit on seat-a", d)
	}
	if b, _ := tp.bindings.Lookup("claude", fableModel, key, testNow); b.Hits < 1 {
		t.Errorf("binding = %+v, want the hit counted", b)
	}
}

func TestPickSwitchesWhenOverrideThresholdOffAndWinnerClearsHysteresis(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML+"affinity:\n  override-threshold: false\n")
	tp.seats(t)
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want the pace winner seat-b", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionFailover || d.PreviousAuthID != "seat-a" || !strings.Contains(d.Note, "hysteresis") {
		t.Errorf("decision = %+v", d)
	}
	if b, _ := tp.bindings.Lookup("claude", fableModel, "k", testNow); b.AuthID != "seat-b" {
		t.Errorf("binding = %+v, want rebound to seat-b", b)
	}
}

func TestPickFailsOverWhenBoundAuthIsAbsent(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	tp.bindings.Bind("claude", fableModel, "k", "seat-gone", testNow)

	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want failover to the pace winner", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionFailover || d.PreviousAuthID != "seat-gone" || d.ChosenAuthID != "seat-b" {
		t.Errorf("decision = %+v", d)
	}
	if b, _ := tp.bindings.Lookup("claude", fableModel, "k", testNow); b.AuthID != "seat-b" {
		t.Errorf("binding = %+v, want rebound to seat-b", b)
	}
}

func TestRetryPickTreatsFailedCredentialAsNotOffered(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	tp.bindings.Bind("claude", fableModel, "k", "seat-b", testNow)

	// The host has already tried seat-b for this request and offers only
	// seat-a; selected_auth_id names the failed credential.
	req := pickRequest(fableModel, "k", "seat-a")
	req.Options.Metadata[MetadataSelectedAuthID] = "seat-b"
	resp := tp.pick(t, req)
	if resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want seat-a", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionFailover || d.PreviousAuthID != "seat-b" || !strings.Contains(d.Note, "retry after seat-b") {
		t.Errorf("decision = %+v", d)
	}
	// A retry pick has the failed credential removed, so its one remaining
	// candidate says nothing about the pool.
	if hasWarning(tp.Status(testNow, ""), "single candidate") {
		t.Error("retry pick raised the single-candidate warning")
	}
}

func TestBoundCredentialBlockedForModelFailsOver(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	blocked := seatA(t)
	blocked.Windows[1].Status = model.StatusRejected
	tp.quota.Put(blocked)
	tp.quota.Put(seatB(t))
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want failover away from the rejected seat-a", resp)
	}
	if d := tp.lastDecision(t); d.Kind != model.DecisionFailover || !strings.Contains(d.Note, "rate-limited") {
		t.Errorf("decision = %+v", d)
	}
}

func TestBoundCredentialBlockedForAnotherFamilyIsKept(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	a := seatA(t)
	// A rejected Opus cap does not bear on a Fable request.
	a.Windows = append(a.Windows, model.Window{
		Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus, Utilization: 1.2, Status: model.StatusRejected,
		ResetsAt: testNow.Add(time.Hour), Duration: model.WeeklyDuration,
	})
	tp.quota.Put(a)
	tp.quota.Put(seatB(t))
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b")); resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want the binding kept", resp)
	}
}

func TestAllStaleWithKeyBindsLeastBound(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	stale := testNow.Add(-time.Hour)
	a, b := seatA(t), seatB(t)
	a.ObservedAt, b.ObservedAt = stale, stale
	tp.quota.Put(a)
	tp.quota.Put(b)
	// seat-a already carries a session; seat-b is idle.
	tp.bindings.Bind("claude", fableModel, "other", "seat-a", testNow)

	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want the least-bound seat-b", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionColdPick || !strings.Contains(d.Note, "least-bound") {
		t.Errorf("decision = %+v", d)
	}
	for _, s := range d.Scores {
		if s.Eligible || s.Reason != model.ReasonStale {
			t.Errorf("score %+v, want ineligible with %s", s, model.ReasonStale)
		}
	}
	if b, _ := tp.bindings.Lookup("claude", fableModel, "k", testNow); b.AuthID != "seat-b" {
		t.Errorf("binding = %+v, want seat-b", b)
	}

	// Ties break on the lowest id, and the fallback only runs with a key.
	if resp := tp.pick(t, pickRequest(fableModel, "k2", "seat-a", "seat-b")); resp.AuthID != "seat-a" {
		t.Errorf("tie-break picked %q, want seat-a", resp.AuthID)
	}
	if resp := tp.pick(t, pickRequest(fableModel, "", "seat-a", "seat-b")); resp.Handled {
		t.Errorf("keyless pick with only stale snapshots handled: %+v", resp)
	}
}

func TestNoSnapshotsWithKeyStillGivesTheSessionAHome(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-b", "seat-a"))
	if !resp.Handled || resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want lowest id seat-a", resp)
	}
	if d := tp.lastDecision(t); len(d.Scores) != 2 || d.Scores[0].Reason != model.ReasonNoSnapshot {
		t.Errorf("scores = %+v, want both marked %s", d.Scores, model.ReasonNoSnapshot)
	}
	// The next request from the same session sticks.
	if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-b", "seat-a")); resp.AuthID != "seat-a" {
		t.Errorf("second pick = %+v, want the same seat", resp)
	}
	if d := tp.lastDecision(t); d.Kind != model.DecisionAffinityHit {
		t.Errorf("second decision = %+v, want affinity-hit", d)
	}
}

func TestSubagentPinsToParent(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	// The parent lives on seat-a, the cold-pick loser.
	tp.bindings.Bind("claude", fableModel, "parent", "seat-a", testNow)

	req := pickRequest(fableModel, "child", "seat-a", "seat-b")
	req.Options.Headers[HeaderSessionParent] = []string{"parent"}
	req.Options.Headers[HeaderSubagent] = []string{"1"}
	resp := tp.pick(t, req)
	if resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want the parent's seat-a", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionAffinityHit || !d.Subagent || !strings.Contains(d.Note, "parent") {
		t.Errorf("decision = %+v", d)
	}
	if b, ok := tp.bindings.Lookup("claude", fableModel, "child", testNow); !ok || b.AuthID != "seat-a" {
		t.Errorf("child binding = %+v ok=%v, want seat-a", b, ok)
	}

	// With subagent pinning off the child is routed on its own merits.
	tp2 := newTestPlugin(t, testConfigYAML+"affinity:\n  subagents: false\n")
	tp2.seats(t)
	tp2.bindings.Bind("claude", fableModel, "parent", "seat-a", testNow)
	if resp := tp2.pick(t, req); resp.AuthID != "seat-b" {
		t.Errorf("subagents=false pick = %+v, want the pace winner", resp)
	}

	// A parent bound to a credential that is not offered falls through.
	tp3 := newTestPlugin(t, testConfigYAML)
	tp3.seats(t)
	tp3.bindings.Bind("claude", fableModel, "parent", "seat-gone", testNow)
	if resp := tp3.pick(t, req); resp.AuthID != "seat-b" {
		t.Errorf("unoffered parent pick = %+v, want the pace winner", resp)
	}
}

func TestSubagentDoesNotInheritABlockedParent(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	blocked := seatA(t)
	blocked.Windows[1].Status = model.StatusRejected
	tp.quota.Put(blocked)
	tp.quota.Put(seatB(t))
	tp.bindings.Bind("claude", fableModel, "parent", "seat-a", testNow)

	req := pickRequest(fableModel, "child", "seat-a", "seat-b")
	req.Options.Headers[HeaderSessionParent] = []string{"parent"}
	req.Options.Headers[HeaderSubagent] = []string{"1"}
	resp := tp.pick(t, req)
	if resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want the child routed away from the rejected seat-a", resp)
	}
	if b, _ := tp.bindings.Lookup("claude", fableModel, "child", testNow); b.AuthID != "seat-b" {
		t.Errorf("child binding = %+v, want seat-b", b)
	}
}

func TestSingleCandidateWarningFollowsTheLatestPick(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.seats(t)
	tp.pick(t, pickRequest(fableModel, "k1", "seat-a"))
	tp.pick(t, pickRequest(fableModel, "k2", "seat-a"))
	if !hasWarning(tp.Status(testNow, ""), "single candidate") {
		t.Error("status warnings lack the single-candidate warning")
	}

	// The log line rides the poll goroutine, because a host call on the pick
	// path has no timeout to unwind it.
	if warned := countWarnings(tp, "single candidate"); warned != 0 {
		t.Errorf("the pick itself logged %d time(s)", warned)
	}
	for i := 0; i < 2; i++ {
		if err := tp.refresh(context.Background()); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}
	if warned := countWarnings(tp, "single candidate"); warned != 1 {
		t.Errorf("single-candidate warning logged %d time(s), want once", warned)
	}

	// A pool that offers more than one candidate again clears the warning.
	tp.pick(t, pickRequest(fableModel, "k3", "seat-a", "seat-b"))
	if hasWarning(tp.Status(testNow, ""), "single candidate") {
		t.Error("the warning outlived the condition it describes")
	}
}

func TestPinnedRequestIsNotASingleCandidatePool(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	req := pickRequest(fableModel, "k", "seat-a")
	req.Options.Metadata[MetadataPinnedAuthID] = "seat-a"
	tp.pick(t, req)
	if hasWarning(tp.Status(testNow, ""), "single candidate") {
		t.Error("a pinned request, which is offered one candidate by design, raised the warning")
	}
}

func TestPickIsSafeUnderConcurrency(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.seats(t)
	// testing only allows Fatalf on the test goroutine, so the workers report
	// through the channel and the assertions run after the join.
	failures := make(chan string, 8)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			key := []string{"k1", "k2", "k3", ""}[i%4]
			payload, err := json.Marshal(pickRequest(fableModel, key, "seat-a", "seat-b"))
			if err != nil {
				failures <- err.Error()
				return
			}
			for j := 0; j < 50; j++ {
				if raw, ok := tp.Call(MethodSchedulerPick, payload); !ok {
					failures <- "pick returned an error envelope: " + string(raw)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	close(failures)
	for msg := range failures {
		t.Error(msg)
	}
	if got := tp.bindings.Len(); got != 3 {
		t.Errorf("bindings = %d, want one per keyed session", got)
	}
}

// hasWarning reports whether any status warning contains substring.
func hasWarning(status model.Status, substring string) bool {
	for _, w := range status.Warnings {
		if strings.Contains(w, substring) {
			return true
		}
	}
	return false
}

// countWarnings counts the warn lines whose message contains substring.
func countWarnings(tp *testPlugin, substring string) int {
	n := 0
	for _, l := range tp.host.logs() {
		if l.Level == "warn" && strings.Contains(l.Message, substring) {
			n++
		}
	}
	return n
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
