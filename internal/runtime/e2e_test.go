package runtime_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	hostSource := strings.TrimSpace(os.Getenv("CPA_SOURCE_DIR"))
	if hostSource == "" {
		t.Skip("set CPA_SOURCE_DIR to a CLIProxyAPI checkout to run the end-to-end test")
	}
	if _, err := os.Stat(filepath.Join(hostSource, "cmd", "server")); err != nil {
		t.Skipf("CPA_SOURCE_DIR %q has no cmd/server: %v", hostSource, err)
	}

	const (
		apiKey        = "e2e-api-key"
		managementKey = "e2e-management-key"
		pluginID      = "claude-seat-pacer"
		modelID       = "claude-sonnet-4-6"
		sessionOne    = "11111111-1111-1111-1111-111111111111"
		sessionTwo    = "22222222-2222-2222-2222-222222222222"
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture proxy refuses upstream", http.StatusBadGateway)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")
	authDir := filepath.Join(dir, "auth")
	for _, path := range []string{pluginDir, authDir, filepath.Join(dir, "home")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	extension, serverName := ".so", "cliproxyapi"
	switch runtime.GOOS {
	case "darwin":
		extension = ".dylib"
	case "windows":
		extension, serverName = ".dll", "cliproxyapi.exe"
	}

	// The plugin id is the library file name, so the build output is named
	// for it.
	buildPlugin := exec.Command("go", "build", "-buildmode=c-shared", "-o", filepath.Join(pluginDir, pluginID+extension), ".")
	buildPlugin.Dir = filepath.Join("..", "..")
	buildPlugin.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := buildPlugin.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, out)
	}

	serverPath := filepath.Join(dir, serverName)
	buildServer := exec.Command("go", "build", "-o", serverPath, "./cmd/server")
	buildServer.Dir = hostSource
	if out, err := buildServer.CombinedOutput(); err != nil {
		t.Fatalf("build CLIProxyAPI: %v\n%s", err, out)
	}

	for index, name := range []string{"claude-a.json", "claude-b.json"} {
		body := fmt.Sprintf(`{"type":"claude","email":"e2e-%d@example.com","access_token":"e2e-token-%d","refresh_token":"e2e-refresh","expired":"2099-01-01T00:00:00Z"}`, index, index)
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	port := unusedTCPPort(t)
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := fmt.Sprintf(`host: "127.0.0.1"
port: %d
proxy-url: %q
auth-dir: %q
api-keys: [%q]
remote-management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
  disable-auto-update-panel: true
logging-to-file: false
debug: false
disable-cooling: true
request-retry: 0
max-retry-credentials: 1
routing:
  session-affinity: false
plugins:
  enabled: true
  dir: %q
  configs:
    %s:
      enabled: true
      priority: 100
      quota:
        poll-interval: 30s
        request-timeout: 2s
`, port, upstream.URL, authDir, apiKey, managementKey, pluginDir, pluginID)
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverPath, "-config", configPath, "-local-model")
	server.Dir = hostSource
	server.Stdout, server.Stderr = logFile, logFile
	server.Env = append(os.Environ(),
		"HOME="+filepath.Join(dir, "home"),
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
		"NO_PROXY=127.0.0.1,localhost",
	)
	if err = server.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	go func() {
		_ = server.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		select {
		case <-processDone:
		default:
			_ = server.Process.Kill()
			<-processDone
		}
		_ = logFile.Close()
		if t.Failed() {
			t.Logf("server log:\n%s", readFile(logPath))
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 10 * time.Second}
	waitForModel(t, client, baseURL, apiKey, processDone, logPath, modelID)

	send := func(sessionID string) {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"metadata":{"user_id":"user_e2e_account_e2e_session_%s"},"messages":[{"role":"user","content":"ping"}]}`, modelID, sessionID)
		request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+apiKey)
		request.Header.Set("Anthropic-Version", "2023-06-01")
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("post /v1/messages: %v\nserver log:\n%s", err, readFile(logPath))
		}
		_ = response.Body.Close()
	}
	status := func() model.Status {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, baseURL+"/v0/management/plugins/"+pluginID+"/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+managementKey)
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("get status: %v", err)
		}
		raw, _ := readAll(response)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status route: %d %s\nserver log:\n%s", response.StatusCode, raw, readFile(logPath))
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
	waitForPoll(t, status, processDone, logPath)
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

	// The resource routes reach the status app without the management key.
	resource, err := client.Get(baseURL + "/v0/resource/plugins/" + pluginID + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := readAll(resource)
	if resource.StatusCode != http.StatusOK {
		t.Fatalf("resource status route: %d %s", resource.StatusCode, raw)
	}
	var served model.Status
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatalf("decode resource status: %v\n%s", err, raw)
	}
	if len(served.Auths) != 2 || len(served.Bindings) != 2 {
		t.Errorf("resource status = %d auths %d bindings, want the same view as the management route", len(served.Auths), len(served.Bindings))
	}
	// The route is unauthenticated, so it publishes a hashed credential id.
	rows := make(map[string]bool, len(served.Auths))
	for _, row := range served.Auths {
		rows[row.AuthID] = true
	}
	for _, row := range second.Auths {
		if quoted := `auth_id":"` + row.AuthID + `"`; bytes.Contains(raw, []byte(quoted)) {
			t.Errorf("the unauthenticated route serves the real id %q: %s", row.AuthID, raw)
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

	page, err := client.Get(baseURL + "/v0/resource/plugins/" + pluginID + "/index.html")
	if err != nil {
		t.Fatal(err)
	}
	pageBody, _ := readAll(page)
	if page.StatusCode != http.StatusOK || !bytes.Contains(pageBody, []byte("<html")) {
		t.Errorf("resource index route: %d %d bytes, want the status page", page.StatusCode, len(pageBody))
	}

	// Nothing that reached the host log carries credential material.
	serverLog := readFile(logPath)
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
