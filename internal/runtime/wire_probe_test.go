package runtime_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
// WIRE.md carries the invocation, including the environment the host build
// needs.
func TestHeaderBridgeSurvivesToSchedulerPick(t *testing.T) {
	hostSource := requireHostSource(t)

	const (
		pluginID = "cpa-probe"
		model    = "claude-sonnet-4-6"
	)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "probe-state.jsonl")

	// CPA_CAPTURE_DIR refreshes testdata/host-payloads from a live host.
	captureDir := strings.TrimSpace(os.Getenv("CPA_CAPTURE_DIR"))
	if captureDir == "" {
		captureDir = filepath.Join(dir, "capture")
	}
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		t.Fatal(err)
	}

	host := startHost(t, hostOptions{
		hostSource:       hostSource,
		dir:              dir,
		pluginID:         pluginID,
		pluginPackage:    "./testdata/probe",
		pluginLDFlags:    "-X=main.stateFile=" + statePath + " -X=main.captureDir=" + captureDir,
		credentialPrefix: "probe",
		pluginSettings:   "      probe-marker: header-bridge\n",
		modelID:          model,
		clientTimeout:    5 * time.Second,
		extraLogs:        map[string]string{"probe state": statePath},
	})

	// The refusing upstream also answers the probe's host.http.do target,
	// which is why the report asserts on 502.
	host.post(t, fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, model))

	var entries []map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		entries = readProbeState(t, statePath)
		if countHook(entries, "scheduler.pick") > 0 {
			// The pick that ended the wait may not be the last one the
			// request produces, so the log settles before it is read whole.
			time.Sleep(250 * time.Millisecond)
			entries = readProbeState(t, statePath)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	injected := map[string]string{}
	for _, entry := range entries {
		if entry["hook"] != "request.intercept_before" {
			continue
		}
		requestID, _ := entry["request_id"].(string)
		marker, _ := entry["marker"].(string)
		if requestID != "" && marker != "" {
			injected[requestID] = marker
		}
	}
	if len(injected) == 0 {
		t.Fatalf("probe never ran request.intercept_before\nprobe state:\n%s\nserver log:\n%s", readFile(statePath), readFile(host.logPath))
	}

	picks := 0
	for _, entry := range entries {
		if entry["hook"] != "scheduler.pick" {
			continue
		}
		picks++
		// scheduler.pick carries no request id, so a pick is tied back to its
		// own interceptor through the request id the marker embeds. A retry
		// picks again for the same request, and two requests interleave.
		seen, _ := entry["marker"].(string)
		want, ok := injected[markerRequestID(seen)]
		if !ok || seen != want {
			t.Fatalf("header bridge REFUTED: scheduler.pick saw %q, injected markers by request %v\nheader keys at pick: %v\nprobe state:\n%s",
				seen, injected, entry["header_keys"], readFile(statePath))
		}
	}
	if picks == 0 {
		t.Fatalf("scheduler.pick was never called, so the bridge is untested\nprobe state:\n%s\nserver log:\n%s", readFile(statePath), readFile(host.logPath))
	}
	t.Logf("header bridge confirmed over %d pick(s); probe state:\n%s", picks, readFile(statePath))

	// The probe mirrors both hooks through host.log, so the server log is the
	// second, independent record of the same round trip.
	hostLogLines := grepLines(readFile(host.logPath), "probe scheduler_pick")
	if len(hostLogLines) == 0 {
		t.Errorf("host.log callback produced no scheduler_pick line\nserver log:\n%s", readFile(host.logPath))
	}
	t.Logf("host.log lines:\n%s", strings.Join(hostLogLines, "\n"))

	// The probe's management route exercises host.auth.list, host.auth.get and
	// host.http.do and returns what they produced, so one call checks four
	// shapes at once.
	reportURL := host.baseURL + "/v0/management/probe/report?http_probe=" + url.QueryEscape(host.upstreamURL+"/probe")
	report, err := http.NewRequest(http.MethodGet, reportURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	report.Header.Set("Authorization", "Bearer "+host.managementKey)
	reportResponse, err := host.client.Do(report)
	if err != nil {
		t.Fatalf("get probe management route: %v", err)
	}
	reportBody, _ := readAll(reportResponse)
	if reportResponse.StatusCode != http.StatusOK {
		t.Fatalf("probe management route: status=%d body=%s\nserver log:\n%s", reportResponse.StatusCode, reportBody, readFile(host.logPath))
	}
	t.Logf("management.handle report:\n%s", reportBody)

	var observed struct {
		AuthList      []map[string]string `json:"auth_list"`
		AuthListError string              `json:"auth_list_error"`
		AuthGet       struct {
			AuthIndex  string   `json:"auth_index"`
			JSONFields []string `json:"json_fields"`
			Error      string   `json:"error"`
		} `json:"auth_get"`
		HTTPDo struct {
			StatusCode int    `json:"status_code"`
			Error      string `json:"error"`
		} `json:"http_do"`
	}
	if err = json.Unmarshal(reportBody, &observed); err != nil {
		t.Fatalf("decode probe report: %v\nbody: %s", err, reportBody)
	}
	if observed.AuthListError != "" || len(observed.AuthList) == 0 {
		t.Errorf("host.auth.list produced no files: %s", reportBody)
	}
	if observed.AuthGet.Error != "" || observed.AuthGet.AuthIndex == "" || len(observed.AuthGet.JSONFields) == 0 {
		t.Errorf("host.auth.get returned no credential JSON: %s", reportBody)
	}
	// host.http.do reaches the fixture, which refuses everything with 502, so
	// the status proves the round trip carried a real response back.
	if observed.HTTPDo.Error != "" || observed.HTTPDo.StatusCode != http.StatusBadGateway {
		t.Errorf("host.http.do did not round-trip: %s", reportBody)
	}
}

// markerRequestID recovers the request id the probe embeds in a marker, which
// it formats as probe-<n>-<request id>.
func markerRequestID(marker string) string {
	parts := strings.SplitN(marker, "-", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[2]
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
	lines := strings.Split(string(raw), "\n")
	out := make([]map[string]any, 0, len(lines))
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entry := map[string]any{}
		if err = json.Unmarshal([]byte(line), &entry); err != nil {
			// This runs while the probe is still appending, so the last line
			// can be a half-written record. Any earlier one is a real defect.
			if index == len(lines)-1 {
				continue
			}
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
