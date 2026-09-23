package runtime_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// TestE2EStickyRoutingThroughRealHost builds this plugin as a shared library,
// runs a real CLIProxyAPI against it with two fabricated Claude OAuth
// credentials and an upstream that refuses every connection, and asserts
// through the plugin's own management route that one Claude Code session is
// routed to one credential across requests while a second session lands on the
// other one.
//
// The host is configured for exactly one pick per request (request-retry 0,
// max-retry-credentials 1), because a refused upstream would otherwise make
// the host try the other credential inside the same request and the retry
// pick would legitimately fail the session over.
//
// No usage endpoint is reachable, so no snapshot is ever eligible and every
// cold pick falls back on the fewest conversations: the first session goes to the lowest
// id, the second to the other credential. Skips unless CPA_SOURCE_DIR points
// at a CLIProxyAPI checkout.
//
//	CPA_SOURCE_DIR=/path/to/CLIProxyAPI GOTOOLCHAIN=auto go test ./internal/runtime -run E2E -v
func TestE2EStickyRoutingThroughRealHost(t *testing.T) {
	hostSource := requireHostSource(t)

	const (
		pluginID   = "claude-seat-pacer"
		modelID    = "claude-sonnet-4-6"
		sessionOne = "11111111-1111-1111-1111-111111111111"
		sessionTwo = "22222222-2222-2222-2222-222222222222"
	)

	host := startHost(t, hostOptions{
		hostSource:       hostSource,
		dir:              t.TempDir(),
		pluginID:         pluginID,
		pluginPackage:    ".",
		pluginBuildDir:   filepath.Join("..", ".."),
		credentialPrefix: "e2e",
		// One pick per request: a refused upstream would otherwise make the
		// host try the other credential inside the same request, and that
		// retry pick would legitimately fail the session over.
		hostSettings: `request-retry: 0
max-retry-credentials: 1
routing:
  session-affinity: false
`,
		pluginSettings: `      quota:
        poll-interval: 30s
        request-timeout: 2s
`,
		modelID:       modelID,
		clientTimeout: 10 * time.Second,
	})

	send := func(sessionID string) {
		t.Helper()
		host.post(t, fmt.Sprintf(`{"model":%q,"max_tokens":16,"metadata":{"user_id":"user_e2e_account_e2e_session_%s"},"messages":[{"role":"user","content":"ping"}]}`, modelID, sessionID))
	}
	status := func() model.Status {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, host.baseURL+"/v0/management/plugins/"+pluginID+"/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+host.managementKey)
		response, err := host.client.Do(request)
		if err != nil {
			t.Fatalf("get status: %v", err)
		}
		raw, _ := readAll(response)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status route: %d %s\nserver log:\n%s", response.StatusCode, raw, readFile(host.logPath))
		}
		var out model.Status
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode status: %v\n%s", err, raw)
		}
		return out
	}

	for i := 0; i < 3; i++ {
		send(sessionOne)
	}
	first := status()
	// Decisions arrive newest first; the assertions read them in order.
	chronological := make([]model.Decision, 0, len(first.Decisions))
	for i := len(first.Decisions) - 1; i >= 0; i-- {
		chronological = append(chronological, first.Decisions[i])
	}
	if len(chronological) != 3 {
		t.Fatalf("decisions = %d, want exactly one per request:\n%s", len(chronological), mustIndent(t, chronological))
	}
	kinds := make([]string, 0, 3)
	for _, d := range chronological {
		kinds = append(kinds, d.Kind)
		if d.Provider != "claude" || d.Model != modelID || d.SessionKey == "" || d.SessionKey != chronological[0].SessionKey {
			t.Errorf("decision %+v does not belong to the same claude session", d)
		}
		if d.ChosenAuthID != chronological[0].ChosenAuthID {
			t.Errorf("decision chose %q, want every request on %q", d.ChosenAuthID, chronological[0].ChosenAuthID)
		}
	}
	if want := []string{model.DecisionColdPick, model.DecisionAffinityHit, model.DecisionAffinityHit}; strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("decision kinds = %v, want %v", kinds, want)
	}
	if chronological[0].ChosenAuthID != "claude-a.json" {
		t.Errorf("first session landed on %q, want the lowest id claude-a.json, which carries the fewest conversations", chronological[0].ChosenAuthID)
	}
	t.Logf("session one: kinds=%v auth=%s key=%s", kinds, chronological[0].ChosenAuthID, chronological[0].SessionKey)

	send(sessionTwo)
	// Credential rows come from the poll loop, whose first tick lands a couple
	// of seconds after registration, so the routing assertions above run before
	// there is anything to show for the credentials themselves.
	waitForPoll(t, status, host.done, host.logPath)
	second := status()
	if len(second.Decisions) != 4 {
		t.Fatalf("decisions after a second session = %d, want 4:\n%s", len(second.Decisions), mustIndent(t, second.Decisions))
	}
	newest := second.Decisions[0]
	if newest.Kind != model.DecisionColdPick || newest.SessionKey == chronological[0].SessionKey || newest.SessionKey == "" {
		t.Errorf("second session decision = %+v, want a cold pick under a new key", newest)
	}
	if newest.ChosenAuthID != "claude-b.json" {
		t.Errorf("second session landed on %q, want the idle claude-b.json", newest.ChosenAuthID)
	}
	if len(second.Bindings) != 2 {
		t.Errorf("bindings = %+v, want one per session", second.Bindings)
	}
	if len(second.Auths) != 2 {
		t.Errorf("auth rows = %+v, want both credentials", second.Auths)
	}
	for _, row := range second.Auths {
		if row.Label == "" || strings.Contains(row.Label, "token") {
			t.Errorf("row %s label = %q", row.AuthID, row.Label)
		}
	}
	t.Logf("session two: kind=%s auth=%s key=%s; bindings=%d", newest.Kind, newest.ChosenAuthID, newest.SessionKey, len(second.Bindings))
	t.Logf("warnings: %v", second.Warnings)

	// No resource route carries data: the host serves resource routes with no
	// key, and an undeclared path never reaches the plugin.
	resource, err := host.client.Get(host.baseURL + "/v0/resource/plugins/" + pluginID + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := readAll(resource); resource.StatusCode != http.StatusNotFound {
		t.Errorf("resource api/status = %d %s, want 404", resource.StatusCode, body)
	}

	// The page's data route is a management route: the host refuses it
	// without the key. One refusal only: the host bans an address after five.
	pageStatusURL := host.baseURL + "/v0/management/plugins/" + pluginID + "/page-status"
	keyless, err := host.client.Get(pageStatusURL)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := readAll(keyless); keyless.StatusCode != http.StatusUnauthorized {
		t.Errorf("page-status without the key = %d %s, want 401", keyless.StatusCode, body)
	}
	request, err := http.NewRequest(http.MethodGet, pageStatusURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+host.managementKey)
	keyed, err := host.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := readAll(keyed)
	if keyed.StatusCode != http.StatusOK {
		t.Fatalf("page-status route: %d %s", keyed.StatusCode, raw)
	}
	var served model.Status
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatalf("decode page status: %v\n%s", err, raw)
	}
	if len(served.Auths) != 2 || len(served.Bindings) != 2 {
		t.Errorf("page status = %d auths %d bindings, want the same view as the status route", len(served.Auths), len(served.Bindings))
	}
	// The page names each seat by a hashed credential id, so it is safe to
	// show on a screen.
	rows := make(map[string]bool, len(served.Auths))
	for _, row := range served.Auths {
		rows[row.AuthID] = true
	}
	for _, row := range second.Auths {
		if quoted := `auth_id":"` + row.AuthID + `"`; bytes.Contains(raw, []byte(quoted)) {
			t.Errorf("the page's route serves the real id %q: %s", row.AuthID, raw)
		}
	}
	// The join every table on the page makes.
	for _, b := range served.Bindings {
		if !rows[b.AuthID] {
			t.Errorf("binding on %q has no credential row: %v", b.AuthID, served.Auths)
		}
	}
	for _, d := range served.Decisions {
		if d.ChosenAuthID != "" && !rows[d.ChosenAuthID] {
			t.Errorf("decision chose %q, which has no credential row: %v", d.ChosenAuthID, served.Auths)
		}
	}

	page, err := host.client.Get(host.baseURL + "/v0/resource/plugins/" + pluginID + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := readAll(page)
	if page.StatusCode != http.StatusOK || !bytes.Contains(pageBody, []byte("<html")) {
		t.Errorf("resource index route: %d %d bytes, want the status page", page.StatusCode, len(pageBody))
	}

	// Nothing that reached the host log carries credential material.
	serverLog := readFile(host.logPath)
	for _, secret := range []string{"e2e-token", "e2e-refresh"} {
		if strings.Contains(serverLog, secret) {
			t.Errorf("server log contains %q", secret)
		}
	}
}

// waitForPoll blocks until the status view carries the credentials the host
// lists, which is what the plugin's first poll publishes.
func waitForPoll(t *testing.T, status func() model.Status, processDone <-chan struct{}, logPath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-processDone:
			t.Fatalf("CLIProxyAPI exited before the first poll\nserver log:\n%s", readFile(logPath))
		default:
		}
		rows := status().Auths
		if len(rows) == 2 && rows[0].Label != "" && rows[1].Label != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the plugin never polled its credentials\nserver log:\n%s", readFile(logPath))
}

func mustIndent(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
