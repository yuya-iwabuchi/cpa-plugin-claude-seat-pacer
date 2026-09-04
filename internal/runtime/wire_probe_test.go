package runtime_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHeaderBridgeSurvivesToSchedulerPick drives a real CLIProxyAPI process
// with the probe plugin from testdata and asserts that a header the probe
// injects in request.intercept_before arrives in SchedulerOptions.Headers at
// the same plugin's scheduler.pick.
//
// The host source is not vendored, so the test skips unless CPA_SOURCE_DIR
// points at a CLIProxyAPI checkout. Upstream traffic is forced through a
// fixture proxy that refuses every connection, and the credentials are
// fabricated, so no provider is contacted and no real credential is used.
//
//	CPA_SOURCE_DIR=~/dev/.research-cpa/CLIProxyAPI go test ./internal/runtime -run HeaderBridge -v
func TestHeaderBridgeSurvivesToSchedulerPick(t *testing.T) {
	hostSource := strings.TrimSpace(os.Getenv("CPA_SOURCE_DIR"))
	if hostSource == "" {
		t.Skip("set CPA_SOURCE_DIR to a CLIProxyAPI checkout to run the header-bridge probe")
	}
	if _, err := os.Stat(filepath.Join(hostSource, "cmd", "server")); err != nil {
		t.Skipf("CPA_SOURCE_DIR %q has no cmd/server: %v", hostSource, err)
	}

	const (
		apiKey        = "probe-api-key"
		managementKey = "probe-management-key"
		pluginID      = "cpa-probe"
		model         = "claude-sonnet-4-6"
	)

	// Every upstream dial lands here and is refused, so the request fails
	// after auth selection instead of reaching a provider.
	upstreamHits := make(chan string, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case upstreamHits <- r.Method + " " + r.Host:
		default:
		}
		http.Error(w, "fixture proxy refuses upstream", http.StatusBadGateway)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugins")
	authDir := filepath.Join(dir, "auth")
	statePath := filepath.Join(dir, "probe-state.jsonl")
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

	// CPA_CAPTURE_DIR refreshes testdata/host-payloads from a live host.
	captureDir := strings.TrimSpace(os.Getenv("CPA_CAPTURE_DIR"))
	if captureDir == "" {
		captureDir = filepath.Join(dir, "capture")
	}
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		t.Fatal(err)
	}

	probePath := filepath.Join(pluginDir, pluginID+extension)
	buildProbe := exec.Command("go", "build", "-buildmode=c-shared",
		"-ldflags", "-X=main.stateFile="+statePath+" -X=main.captureDir="+captureDir,
		"-o", probePath, "./testdata/probe")
	buildProbe.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := buildProbe.CombinedOutput(); err != nil {
		t.Fatalf("build probe plugin: %v\n%s", err, out)
	}

	serverPath := filepath.Join(dir, serverName)
	buildServer := exec.Command("go", "build", "-o", serverPath, "./cmd/server")
	buildServer.Dir = hostSource
	if out, err := buildServer.CombinedOutput(); err != nil {
		t.Fatalf("build CLIProxyAPI: %v\n%s", err, out)
	}

	for index, name := range []string{"claude-a.json", "claude-b.json"} {
		body := fmt.Sprintf(`{"type":"claude","email":"probe-%d@example.com","access_token":"probe-token-%d","refresh_token":"probe-refresh","expired":"2099-01-01T00:00:00Z"}`, index, index)
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
plugins:
  enabled: true
  dir: %q
  configs:
    %s:
      enabled: true
      priority: 100
      probe-marker: header-bridge
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
			t.Logf("probe state:\n%s", readFile(statePath))
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}
	waitForModel(t, client, baseURL, apiKey, processDone, logPath, model)

	body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, model)
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

	var entries []map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		entries = readProbeState(t, statePath)
		if countHook(entries, "scheduler.pick") > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	injected := ""
	for _, entry := range entries {
		if entry["hook"] == "request.intercept_before" {
			injected, _ = entry["marker"].(string)
		}
	}
	if injected == "" {
		t.Fatalf("probe never ran request.intercept_before\nprobe state:\n%s\nserver log:\n%s", readFile(statePath), readFile(logPath))
	}

	picks := 0
	for _, entry := range entries {
		if entry["hook"] != "scheduler.pick" {
			continue
		}
		picks++
		seen, _ := entry["marker"].(string)
		if seen != injected {
			t.Fatalf("header bridge REFUTED: intercept_before injected %q, scheduler.pick saw %q\nheader keys at pick: %v\nprobe state:\n%s",
				injected, seen, entry["header_keys"], readFile(statePath))
		}
	}
	if picks == 0 {
		t.Fatalf("scheduler.pick was never called, so the bridge is untested\nprobe state:\n%s\nserver log:\n%s", readFile(statePath), readFile(logPath))
	}
	t.Logf("header bridge confirmed over %d pick(s); probe state:\n%s", picks, readFile(statePath))

	// The probe mirrors both hooks through host.log, so the server log is the
	// second, independent record of the same round trip.
	hostLogLines := grepLines(readFile(logPath), "probe scheduler_pick")
	if len(hostLogLines) == 0 {
		t.Errorf("host.log callback produced no scheduler_pick line\nserver log:\n%s", readFile(logPath))
	}
	t.Logf("host.log lines:\n%s", strings.Join(hostLogLines, "\n"))

	// The probe's management route exercises host.auth.list and host.auth.get
	// and returns what they produced, so one call checks three shapes at once.
	reportURL := baseURL + "/v0/management/probe/report?http_probe=" + url.QueryEscape(upstream.URL+"/probe")
	report, err := http.NewRequest(http.MethodGet, reportURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	report.Header.Set("Authorization", "Bearer "+managementKey)
	reportResponse, err := client.Do(report)
	if err != nil {
		t.Fatalf("get probe management route: %v", err)
	}
	reportBody, _ := readAll(reportResponse)
	if reportResponse.StatusCode != http.StatusOK {
		t.Fatalf("probe management route: status=%d body=%s\nserver log:\n%s", reportResponse.StatusCode, reportBody, readFile(logPath))
	}
	t.Logf("management.handle report:\n%s", reportBody)
	if !bytes.Contains(reportBody, []byte(`"auth_list"`)) {
		t.Errorf("host.auth.list produced no files: %s", reportBody)
	}
	if bytes.Contains(reportBody, []byte(`"auth_list_error"`)) {
		t.Errorf("host.auth.list failed: %s", reportBody)
	}
}

func grepLines(text, needle string) []string {
	out := make([]string, 0, 4)
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

func readProbeState(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, 8)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entry := map[string]any{}
		if err = json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("probe wrote an unparsable state line %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

func countHook(entries []map[string]any, hook string) int {
	total := 0
	for _, entry := range entries {
		if entry["hook"] == hook {
			total++
		}
	}
	return total
}

func waitForModel(t *testing.T, client *http.Client, baseURL, apiKey string, processDone <-chan struct{}, logPath, model string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-processDone:
			t.Fatalf("CLIProxyAPI exited before registering %s\nserver log:\n%s", model, readFile(logPath))
		default:
		}
		request, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+apiKey)
		response, err := client.Do(request)
		if err == nil {
			raw, _ := readAll(response)
			if response.StatusCode == http.StatusOK && bytes.Contains(raw, []byte(model)) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("CLIProxyAPI never registered %s\nserver log:\n%s", model, readFile(logPath))
}

func readAll(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	var buffer bytes.Buffer
	_, err := buffer.ReadFrom(response.Body)
	return buffer.Bytes(), err
}

func unusedTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func readFile(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}
