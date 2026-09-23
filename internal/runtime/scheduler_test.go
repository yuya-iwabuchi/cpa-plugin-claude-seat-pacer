package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/pace"
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

func TestEverySeatRejectedKeepsTheBinding(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	for _, snap := range []model.AuthSnapshot{seatA(t), seatB(t)} {
		snap.Windows[1].Status = model.StatusRejected
		tp.quota.Put(snap)
	}
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	// Failing over between two rejected seats costs a cross-org cache miss
	// per request and serves none of them, so the seat holds across requests.
	for i := 0; i < 3; i++ {
		resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
		if resp.AuthID != "seat-a" {
			t.Fatalf("pick %d = %+v, want the binding kept on seat-a", i, resp)
		}
		d := tp.lastDecision(t)
		if d.Kind != model.DecisionAffinityHit || !strings.Contains(d.Note, "no other seat can take this model") {
			t.Fatalf("decision %d = %+v", i, d)
		}
	}
	if b, _ := tp.bindings.Lookup("claude", fableModel, "k", testNow); b.AuthID != "seat-a" {
		t.Errorf("binding = %+v, want seat-a", b)
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
		t.Fatalf("response = %+v, want seat-b, which carries the fewest conversations", resp)
	}
	d := tp.lastDecision(t)
	if d.Kind != model.DecisionColdPick || !strings.Contains(d.Note, "fewest live conversations") {
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

// TestFallbackPrefersAStaleSeatOverARefusedOne covers the fewest-conversations
// fallback when a candidate the provider will not serve carries fewer
// conversations than one whose reading is merely too old to trust.
func TestFallbackPrefersAStaleSeatOverARefusedOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(*model.AuthSnapshot)
	}{
		{"rejected", func(s *model.AuthSnapshot) { s.Windows[1].Status = model.StatusRejected }},
		{"spent", func(s *model.AuthSnapshot) { s.Windows[1].Utilization = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, testConfigYAML)
			a, b := seatA(t), seatB(t)
			tc.refuse(&a)
			b.ObservedAt = testNow.Add(-time.Hour)
			tp.quota.Put(a)
			tp.quota.Put(b)
			tp.bindings.Bind("claude", fableModel, "other-1", "seat-b", testNow)
			tp.bindings.Bind("claude", fableModel, "other-2", "seat-b", testNow)

			resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
			if resp.AuthID != "seat-b" {
				t.Fatalf("cold pick = %+v, want the stale seat-b over the %s seat-a", resp, tc.name)
			}
		})
	}

	// A binding the provider refused fails over, and the fallback agrees with
	// hasAlternativeHome that the stale seat is the better home.
	tp := newTestPlugin(t, testConfigYAML)
	a, b := seatA(t), seatB(t)
	a.Windows[1].Status = model.StatusRejected
	b.ObservedAt = testNow.Add(-time.Hour)
	tp.quota.Put(a)
	tp.quota.Put(b)
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)
	tp.bindings.Bind("claude", fableModel, "other-1", "seat-b", testNow)
	tp.bindings.Bind("claude", fableModel, "other-2", "seat-b", testNow)

	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want failover to the stale seat-b", resp)
	}
	if d := tp.lastDecision(t); d.Kind != model.DecisionFailover || d.PreviousAuthID != "seat-a" {
		t.Errorf("decision = %+v, want a failover away from seat-a", d)
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

// TestSoleRejectedCandidateKeepsTheBindingAndNamesTheRejection covers a
// binding whose credential is the only one offered: there is nowhere to move
// to, so the seat holds, and the provider's own rejection is what the log
// reads rather than a bare keep.
func TestSoleRejectedCandidateKeepsTheBindingAndSaysItIsTheOnlySeat(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	blocked := seatA(t)
	blocked.Windows[1].Status = model.StatusRejected
	tp.quota.Put(blocked)
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a")); !resp.Handled || resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want the binding kept on seat-a", resp)
	}
	d := tp.lastDecision(t)
	if !strings.Contains(d.Note, "it is the only seat offered") {
		t.Errorf("note = %q, want the single-candidate pool named", d.Note)
	}
	if d.Kind != model.DecisionAffinityHit {
		t.Errorf("decision kind = %q, want %q", d.Kind, model.DecisionAffinityHit)
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

// A credential the provider reports as full cannot take a new conversation.
// pace.ScoreAuth gates it, and the cold pick has to honour that gate rather
// than route to a seat with nothing left in the window the request needs.
func TestColdPickSkipsASpentSeat(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	spent := seatB(t)
	spent.Windows[2].Utilization = 1.0 // the Fable cap this request needs
	tp.quota.Put(seatA(t))
	tp.quota.Put(spent)

	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want the seat that still holds Fable budget", resp)
	}
	// A full Fable cap bears on no Standard request, so the seat carrying it is
	// still a candidate for one — and still the pace winner, since its weekly
	// windows are untouched.
	before := pace.ScoreAuth(tp.config().Pace, spent, "claude-sonnet-4-5", testNow)
	if !before.Eligible || before.Reason != model.ReasonEligible {
		t.Fatalf("seat-b scored %+v for Standard, want eligible despite a full Fable cap", before)
	}
	resp = tp.pick(t, pickRequest("claude-sonnet-4-5", "standard-key", "seat-a", "seat-b"))
	if !resp.Handled || resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want seat-b: a full Fable cap must not bar a Standard request", resp)
	}
}

// Moving a refused binding to a seat the provider reports as full buys nothing
// and spends a cross-organization cache miss on the way, so the binding holds.
func TestARefusedBindingDoesNotFailOverToASpentSeat(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	refused := seatA(t)
	refused.Windows[2].Status = model.StatusRejected
	spent := seatB(t)
	spent.Windows[2].Utilization = 1.0
	tp.quota.Put(refused)
	tp.quota.Put(spent)
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	for i := 0; i < 3; i++ {
		resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
		if resp.AuthID != "seat-a" {
			t.Fatalf("pick %d = %+v, want the binding kept rather than moved to a spent seat", i, resp)
		}
	}
	if b, _ := tp.bindings.Lookup("claude", fableModel, "k", testNow); b.AuthID != "seat-a" {
		t.Errorf("binding = %+v, want seat-a", b)
	}
}

// Affinity outranks fullness: with Affinity.OverrideThreshold on, an
// established conversation stays on its seat even once the provider reports
// the window full, because moving it costs a certain cache miss while the
// reading may be one poll stale. The request is forwarded and the provider
// decides. This pins the behaviour so a change to it is deliberate.
func TestASpentBindingIsKeptWhileAffinityOverridesPace(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	spent := seatA(t)
	spent.Windows[2].Utilization = 1.02
	tp.quota.Put(spent)
	tp.quota.Put(seatB(t))
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	if !tp.config().Affinity.OverrideThreshold {
		t.Fatal("override-threshold is off, so this test no longer covers what it claims")
	}
	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if resp.AuthID != "seat-a" {
		t.Fatalf("response = %+v, want the binding kept on the spent seat", resp)
	}
	if d := tp.lastDecision(t); d.Kind != model.DecisionAffinityHit {
		t.Errorf("decision = %+v, want an affinity hit", d)
	}
	// A conversation that has no binding yet still avoids it.
	if resp := tp.pick(t, pickRequest(fableModel, "fresh", "seat-a", "seat-b")); resp.AuthID != "seat-b" {
		t.Errorf("cold pick = %+v, want the seat with Fable budget", resp)
	}
}

// A reading the plugin has already discarded as too old cannot bar a sibling
// from taking a refused binding. The snapshot map keeps every reading whatever
// its age, so answering this from the snapshot rather than the score pinned a
// conversation to a credential the provider had refused.
func TestAStaleFullReadingDoesNotPinARefusedBinding(t *testing.T) {
	refused := seatA(t)
	refused.Windows[2].Status = model.StatusRejected

	t.Run("a reading past max-staleness leaves the sibling a home", func(t *testing.T) {
		tp := newTestPlugin(t, testConfigYAML)
		stale := seatB(t)
		stale.Windows[2].Utilization = 1.0
		stale.ObservedAt = testNow.Add(-72 * time.Hour)
		tp.quota.Put(refused)
		tp.quota.Put(stale)
		tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

		if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b")); resp.AuthID == "seat-a" {
			t.Fatalf("response = %+v, want a failover: the sibling's reading is too old to bar it", resp)
		}
	})

	t.Run("a current reading at full does bar it", func(t *testing.T) {
		tp := newTestPlugin(t, testConfigYAML)
		full := seatB(t)
		full.Windows[2].Utilization = 1.0
		tp.quota.Put(refused)
		tp.quota.Put(full)
		tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

		if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b")); resp.AuthID != "seat-a" {
			t.Fatalf("response = %+v, want the binding kept: the sibling has nothing left", resp)
		}
	})
}

// A full cap for another family bars nothing here: it applies to no request of
// this family, so the seat carrying it is still somewhere a refused binding can
// move to.
func TestAFullCapForAnotherFamilyStillLeavesASeatAHome(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	refused := seatA(t)
	refused.Windows[2].Status = model.StatusRejected
	other := seatB(t)
	other.Windows = append(other.Windows, model.Window{
		Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus, Utilization: 1.0,
		ResetsAt: testNow.Add(48 * time.Hour), Duration: model.WeeklyDuration,
	})
	tp.quota.Put(refused)
	tp.quota.Put(other)
	tp.bindings.Bind("claude", fableModel, "k", "k", testNow)
	tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

	if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b")); resp.AuthID != "seat-b" {
		t.Fatalf("response = %+v, want seat-b: its full cap is another family's", resp)
	}
}

// Keeping a binding on a credential the provider reports as full is a choice,
// not an oversight, so the decision carries the reason. An ordinary kept seat
// carries none: the status view marks a note on hover, and a note on every hit
// would mark every request of every conversation.
func TestOnlyASpentBindingRecordsWhyItWasKept(t *testing.T) {
	t.Run("a spent binding names the reason", func(t *testing.T) {
		tp := newTestPlugin(t, testConfigYAML)
		spent := seatA(t)
		spent.Windows[2].Utilization = 1.02
		tp.quota.Put(spent)
		tp.quota.Put(seatB(t))
		tp.bindings.Bind("claude", fableModel, "k", "seat-a", testNow)

		if resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b")); resp.AuthID != "seat-a" {
			t.Fatalf("response = %+v, want the binding kept", resp)
		}
		d := tp.lastDecision(t)
		if d.Kind != model.DecisionAffinityHit {
			t.Fatalf("kind = %q, want %q", d.Kind, model.DecisionAffinityHit)
		}
		if !strings.Contains(d.Note, "spent") {
			t.Errorf("note = %q, want the spent seat named", d.Note)
		}
	})

	t.Run("an ordinary kept seat carries no note", func(t *testing.T) {
		tp := newTestPlugin(t, testConfigYAML)
		tp.seats(t)
		tp.bindings.Bind("claude", fableModel, "k", "seat-b", testNow)

		tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
		d := tp.lastDecision(t)
		if d.Kind != model.DecisionAffinityHit {
			t.Fatalf("kind = %q, want %q", d.Kind, model.DecisionAffinityHit)
		}
		if d.Note != "" {
			t.Errorf("note = %q, want none so the status view marks only the deliberate case", d.Note)
		}
	})
}
