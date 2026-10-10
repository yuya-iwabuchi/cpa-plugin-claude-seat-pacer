package runtime_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// TestE2ECooldownResetThroughRealHost runs a real CLIProxyAPI with cooling on
// and three Claude OAuth seats, each reachable alone through its own model
// prefix, and drives every seat into a weekly refusal from a fake Anthropic:
//
//   - sa gets a 429 rejecting the weekly window until its reset; its usage
//     then reads 0% under the same reset, so its cooldown is stale and the
//     plugin clears it, after which a request on sa is served.
//   - sb gets the same 429 and its usage stays at 100%, so its cooldown is
//     legitimate and stands.
//   - sc gets the same 429 plus a Retry-After a day past the weekly reset, so
//     the host cools it past any reset the refusal span expected; its usage
//     clears, and the cooldown still stands.
//
// A host without host.routing.reset_cooldown (before 8.0.12) is asked once,
// keeps every cooldown, and the status view warns about sa.
//
// The host is built with a -overlay that makes a Claude credential with no
// base_url attribute send to CPA_E2E_CLAUDE_BASE_URL: a Claude OAuth file
// carries no base_url, and api.anthropic.com is reached over a uTLS transport
// verified against the system roots. The fixture is also the host's proxy, so
// every other upstream dial it sees is refused.
//
//	CPA_SOURCE_DIR=/path/to/CLIProxyAPI GOTOOLCHAIN=auto go test ./internal/runtime -run TestE2ECooldownReset -v
func TestE2ECooldownResetThroughRealHost(t *testing.T) {
	hostSource := requireHostSource(t)
	resetSupported := hostHasResetCooldown(t, hostSource)

	const (
		pluginID = "claude-seat-pacer"
		modelID  = "claude-sonnet-4-6"
	)
	dir := t.TempDir()
	now := time.Now().UTC()
	fixture := &anthropicFixture{
		weeklyReset:  now.Add(3 * 24 * time.Hour).Truncate(time.Hour),
		sessionReset: now.Add(3 * time.Hour).Truncate(time.Minute),
		seats: map[string]*fixtureSeat{
			"e2e-token-sa": {weekly: 100},
			"e2e-token-sb": {weekly: 100},
			"e2e-token-sc": {weekly: 100},
		},
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)

	credentials := make(map[string]string, 3)
	for _, seat := range []string{"sa", "sb", "sc"} {
		credentials["claude-"+seat+".json"] = fmt.Sprintf(`{"type":"claude","email":"e2e-%s@example.com","access_token":"e2e-token-%s","refresh_token":"e2e-refresh","expired":"2099-01-01T00:00:00Z","prefix":%q}`, seat, seat, seat)
	}

	host := startHost(t, hostOptions{
		hostSource:       hostSource,
		dir:              dir,
		pluginID:         pluginID,
		pluginPackage:    ".",
		pluginBuildDir:   filepath.Join("..", ".."),
		credentials:      credentials,
		proxyURL:         server.URL,
		cooling:          true,
		serverBuildFlags: claudeBaseURLOverlay(t, hostSource, dir),
		serverEnv:        []string{"CPA_E2E_CLAUDE_BASE_URL=" + server.URL},
		hostSettings: `request-retry: 0
max-retry-credentials: 1
routing:
  session-affinity: false
`,
		pluginSettings: fmt.Sprintf(`      quota:
        poll-interval: 30s
        request-timeout: 2s
        persist-history: false
        usage-url: %q
`, server.URL+"/api/oauth/usage"),
		modelID:       modelID,
		clientTimeout: 15 * time.Second,
	})
	api := hostAPI{t: t, host: host, pluginID: pluginID}

	// Every seat is read once before any refusal, so each weekly ring holds
	// the 100% level the refusal arrives at.
	api.refresh()
	for _, row := range api.status().Auths {
		if row.Snapshot.Err != "" || len(row.Snapshot.Windows) == 0 {
			t.Fatalf("seat %s has no usage reading: %+v", row.AuthID, row.Snapshot)
		}
	}

	fixture.set("e2e-token-sa", func(s *fixtureSeat) { s.refuse = true })
	fixture.set("e2e-token-sb", func(s *fixtureSeat) { s.refuse = true })
	fixture.set("e2e-token-sc", func(s *fixtureSeat) { s.refuse, s.retryAfter = true, 24*time.Hour })
	for _, seat := range []string{"sa", "sb", "sc"} {
		if code := api.send(seat+"/"+modelID, seat); code != http.StatusTooManyRequests {
			t.Errorf("request on %s = %d, want the fixture's 429 passed through", seat, code)
		}
	}

	// The refusal reaches the plugin through usage.handle after the response,
	// so the span is waited for rather than read once.
	waitFor(t, host, "an ongoing weekly refusal span on every seat", func() bool {
		rows := api.statusByID()
		for _, seat := range []string{"sa", "sb", "sc"} {
			lock, ok := newestWeeklyLock(rows["claude-"+seat+".json"])
			if !ok || lock.End != "" {
				return false
			}
		}
		return true
	})
	rows := api.statusByID()
	for _, seat := range []string{"sa", "sb", "sc"} {
		lock, _ := newestWeeklyLock(rows["claude-"+seat+".json"])
		if got := time.Unix(lock.To, 0).UTC(); !got.Equal(fixture.weeklyReset) {
			t.Errorf("%s span runs to %s, want the weekly reset %s", seat, got, fixture.weeklyReset)
		}
	}

	held := api.authFiles()
	for seat, want := range map[string]time.Time{
		"sa": fixture.weeklyReset,
		"sb": fixture.weeklyReset,
		"sc": fixture.weeklyReset.Add(24 * time.Hour),
	} {
		entry := held["claude-"+seat+".json"]
		// The host adds up to 30 seconds of fuzz to a Claude reset, and a
		// Retry-After counts from the instant the host read it.
		if !entry.Unavailable || entry.NextRetryAfter.Before(want.Add(-2*time.Minute)) || entry.NextRetryAfter.After(want.Add(2*time.Minute)) {
			t.Errorf("host holds %s unavailable=%v until %s, want unavailable until about %s", seat, entry.Unavailable, entry.NextRetryAfter.UTC(), want)
		}
	}
	t.Logf("host holds: sa until %s, sb until %s, sc until %s",
		held["claude-sa.json"].NextRetryAfter.UTC(), held["claude-sb.json"].NextRetryAfter.UTC(), held["claude-sc.json"].NextRetryAfter.UTC())
	if t.Failed() {
		t.FailNow()
	}

	// The provider gives sa and sc their weekly quota back under the same
	// reset; sb stays full.
	fixture.set("e2e-token-sa", func(s *fixtureSeat) { s.weekly, s.refuse = 0, false })
	fixture.set("e2e-token-sc", func(s *fixtureSeat) { s.weekly = 0 })

	// A clearing takes two reads a second or more apart, and the reset runs
	// on the poll after the second.
	const hostReset = "pluginhost: plugin reset credential cooldown"
	const pluginCleared = "claude-seat-pacer cleared a host cooldown on a seat whose quota is back"
	const pluginUnsupported = "claude-seat-pacer: this host has no host.routing.reset_cooldown"
	settled := func() bool {
		log := readFile(host.logPath)
		if resetSupported {
			return strings.Contains(log, pluginCleared)
		}
		return strings.Contains(log, pluginUnsupported)
	}
	for i := 0; i < 6 && !settled(); i++ {
		time.Sleep(1100 * time.Millisecond)
		api.refresh()
	}
	// One more read: a reset asked for twice would show up here.
	time.Sleep(1100 * time.Millisecond)
	api.refresh()

	rows = api.statusByID()
	for seat, want := range map[string]model.LockEnd{"sa": model.LockEndCleared, "sb": "", "sc": model.LockEndCleared} {
		lock, ok := newestWeeklyLock(rows["claude-"+seat+".json"])
		if !ok || lock.End != want {
			t.Errorf("%s newest weekly span = %+v, want end %q", seat, lock, want)
		}
	}

	serverLog := readFile(host.logPath)
	held = api.authFiles()
	status := api.status()
	var cooledWarnings []string
	for _, w := range status.Warnings {
		if strings.Contains(w, "keeps it cooled") {
			cooledWarnings = append(cooledWarnings, w)
		}
	}
	for _, line := range strings.Split(serverLog, "\n") {
		if strings.Contains(line, hostReset) || strings.Contains(line, "claude-seat-pacer cleared") || strings.Contains(line, "reset_cooldown") || strings.Contains(line, "could not clear") {
			t.Logf("host log: %s", line)
		}
	}
	t.Logf("warnings: %q", status.Warnings)
	for _, seat := range []string{"sa", "sb", "sc"} {
		lock, _ := newestWeeklyLock(rows["claude-"+seat+".json"])
		entry := held["claude-"+seat+".json"]
		t.Logf("%s: span %+v; host unavailable=%v until %s", seat, lock, entry.Unavailable, entry.NextRetryAfter.UTC())
	}

	if resetSupported {
		if n := strings.Count(serverLog, hostReset); n != 1 {
			t.Errorf("host logged %d cooldown resets, want exactly one, for sa", n)
		}
		if n := strings.Count(serverLog, pluginCleared); n != 1 {
			t.Errorf("plugin logged %d cleared cooldowns, want exactly one, for sa", n)
		}
		for _, line := range strings.Split(serverLog, "\n") {
			if strings.Contains(line, hostReset) && !strings.Contains(line, `auth_index="`+held["claude-sa.json"].AuthIndex+`"`) {
				t.Errorf("host reset a cooldown on another seat than sa (auth_index %s): %s", held["claude-sa.json"].AuthIndex, line)
			}
			if strings.Contains(line, pluginCleared) && !strings.Contains(line, "claude-sa.json") {
				t.Errorf("plugin cleared a cooldown on another seat than sa: %s", line)
			}
		}
		if entry := held["claude-sa.json"]; entry.Unavailable && entry.NextRetryAfter.After(time.Now()) {
			t.Errorf("host still holds sa unavailable until %s after the reset", entry.NextRetryAfter.UTC())
		}
		if len(cooledWarnings) != 0 {
			t.Errorf("status warns of a held seat after the reset: %q", cooledWarnings)
		}
		if code := api.send("sa/"+modelID, "sa-after"); code != http.StatusOK {
			t.Errorf("request on sa after the reset = %d, want 200", code)
		}
	} else {
		if strings.Contains(serverLog, hostReset) || strings.Contains(serverLog, pluginCleared) {
			t.Errorf("a host without host.routing.reset_cooldown logged a reset")
		}
		if n := strings.Count(serverLog, pluginUnsupported); n != 1 {
			t.Errorf("plugin logged the missing callback %d times, want once per process", n)
		}
		if entry := held["claude-sa.json"]; !entry.Unavailable {
			t.Errorf("an older host released sa without a reset: %+v", entry)
		}
		if len(cooledWarnings) != 1 || !strings.Contains(cooledWarnings[0], "8.0.12") {
			t.Errorf("status warnings about a held seat = %q, want one naming 8.0.12", cooledWarnings)
		}
		select {
		case <-host.done:
			t.Errorf("CLIProxyAPI exited")
		default:
		}
	}
	for _, seat := range []string{"sb", "sc"} {
		if entry := held["claude-"+seat+".json"]; !entry.Unavailable || !entry.NextRetryAfter.After(time.Now()) {
			t.Errorf("host released %s, whose cooldown is legitimate: %+v", seat, entry)
		}
	}
	t.Logf("fixture: %d message requests per token %v, %d usage reads", fixture.messageCount(), fixture.messagesByToken(), fixture.usageCount())

	for _, secret := range []string{"e2e-token", "e2e-refresh"} {
		if strings.Contains(serverLog, secret) {
			t.Errorf("server log contains %q", secret)
		}
	}
}

// hostHasResetCooldown reports whether the host source declares
// host.routing.reset_cooldown.
func hostHasResetCooldown(t *testing.T, hostSource string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(hostSource, "sdk", "pluginabi", "types.go"))
	if err != nil {
		t.Fatalf("read the host's plugin ABI: %v", err)
	}
	return strings.Contains(string(raw), `"host.routing.reset_cooldown"`)
}

// claudeBaseURLOverlay writes a go build -overlay that makes the host's Claude
// executor fall back to CPA_E2E_CLAUDE_BASE_URL for a credential with no
// base_url attribute, and returns the build flags naming it. The host
// checkout itself is left untouched.
func claudeBaseURLOverlay(t *testing.T, hostSource, dir string) []string {
	t.Helper()
	const marker = `baseURL = a.Attributes["base_url"]`
	original := filepath.Join(hostSource, "internal", "runtime", "executor", "claude_executor_request.go")
	raw, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("read the host's Claude executor: %v", err)
	}
	if strings.Count(string(raw), marker) != 1 {
		t.Fatalf("%s does not read the base_url attribute in one place; the overlay needs updating", original)
	}
	patched := strings.Replace(string(raw), marker, marker+`; if baseURL == "" { baseURL = e2eClaudeBaseURL() }`, 1)
	overlayDir := filepath.Join(dir, "overlay")
	if err := os.MkdirAll(overlayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	patchedPath := filepath.Join(overlayDir, "claude_executor_request.go")
	helperPath := filepath.Join(overlayDir, "e2e_base_url.go")
	if err := os.WriteFile(patchedPath, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := "package executor\n\nimport \"os\"\n\nfunc e2eClaudeBaseURL() string { return os.Getenv(\"CPA_E2E_CLAUDE_BASE_URL\") }\n"
	if err := os.WriteFile(helperPath, []byte(helper), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {
		original: patchedPath,
		filepath.Join(filepath.Dir(original), "zz_e2e_base_url.go"): helperPath,
	}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(overlayDir, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"-overlay", overlayPath}
}

// fixtureSeat is what the fake Anthropic answers for one access token.
type fixtureSeat struct {
	// weekly is the weekly utilization in percent the usage endpoint reports.
	weekly float64
	// refuse answers a Messages request with a 429 rejecting the weekly
	// window until its reset; retryAfter, when set, adds a Retry-After that
	// long past that reset.
	refuse     bool
	retryAfter time.Duration
}

// anthropicFixture is a fake Anthropic API and the host's proxy in one. It
// serves the Messages API and the OAuth usage endpoint by path, whether a
// request arrives directly or through the proxy, and refuses everything else,
// a CONNECT to a real host included.
type anthropicFixture struct {
	weeklyReset, sessionReset time.Time

	mu       sync.Mutex
	seats    map[string]*fixtureSeat
	messages map[string]int
	usage    int
}

func (f *anthropicFixture) set(token string, change func(*fixtureSeat)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.seats[token])
}

func (f *anthropicFixture) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.messages {
		n += c
	}
	return n
}

func (f *anthropicFixture) messagesByToken() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.messages))
	for token, c := range f.messages {
		out[strings.TrimPrefix(token, "e2e-token-")] = c
	}
	return out
}

func (f *anthropicFixture) usageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usage
}

func (f *anthropicFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, "fixture proxy refuses upstream", http.StatusBadGateway)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		token = r.Header.Get("X-Api-Key")
	}
	f.mu.Lock()
	seat, known := f.seats[token]
	var copied fixtureSeat
	if known {
		copied = *seat
	}
	switch r.URL.Path {
	case "/v1/messages":
		if f.messages == nil {
			f.messages = make(map[string]int)
		}
		f.messages[token]++
	case "/api/oauth/usage":
		f.usage++
	}
	f.mu.Unlock()

	switch {
	case !known:
		http.Error(w, "fixture knows no such token", http.StatusUnauthorized)
	case r.URL.Path == "/api/oauth/usage" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"five_hour":{"utilization":10.0,"resets_at":%q},"seven_day":{"utilization":%.1f,"resets_at":%q}}`,
			f.sessionReset.Format(time.RFC3339), copied.weekly, f.weeklyReset.Format(time.RFC3339))
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		f.messagesResponse(w, r, copied)
	default:
		http.Error(w, "fixture serves no such path", http.StatusBadGateway)
	}
}

func (f *anthropicFixture) messagesResponse(w http.ResponseWriter, r *http.Request, seat fixtureSeat) {
	var body struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	h := w.Header()
	weeklyReset := strconv.FormatInt(f.weeklyReset.Unix(), 10)
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(f.sessionReset.Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", weeklyReset)
	h.Set("Anthropic-Ratelimit-Unified-Reset", weeklyReset)
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "seven_day")
	if seat.refuse {
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		h.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
		h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "1.0")
		if seat.retryAfter > 0 {
			h.Set("Retry-After", strconv.FormatInt(int64(time.Until(f.weeklyReset.Add(seat.retryAfter))/time.Second), 10))
		}
		h.Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`)
		return
	}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", strconv.FormatFloat(seat.weekly/100, 'f', 2, 64))
	if !body.Stream {
		h.Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
		return
	}
	h.Set("Content-Type", "text/event-stream")
	for _, event := range []struct{ name, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`},
		{"message_stop", `{"type":"message_stop"}`},
	} {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, event.data)
	}
}

func (a hostAPI) statusByID() map[string]model.AuthStatus {
	a.t.Helper()
	rows := make(map[string]model.AuthStatus)
	for _, row := range a.status().Auths {
		rows[row.AuthID] = row
	}
	return rows
}

// heldAuth is the cooldown state the host's management API reports for one
// credential file.
type heldAuth struct {
	Name           string    `json:"name"`
	AuthIndex      string    `json:"auth_index"`
	Unavailable    bool      `json:"unavailable"`
	NextRetryAfter time.Time `json:"next_retry_after"`
}

func (a hostAPI) authFiles() map[string]heldAuth {
	a.t.Helper()
	raw := a.managementDo(http.MethodGet, "/auth-files")
	var out struct {
		Files []heldAuth `json:"files"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		a.t.Fatalf("decode auth-files: %v\n%s", err, raw)
	}
	files := make(map[string]heldAuth, len(out.Files))
	for _, f := range out.Files {
		files[f.Name] = f
	}
	return files
}

// newestWeeklyLock is the newest refusal span on a seat's all-models weekly
// window.
func newestWeeklyLock(row model.AuthStatus) (model.Lock, bool) {
	for _, h := range row.History {
		if h.Kind == model.WindowWeekly && h.Scope == "" && len(h.Locks) > 0 {
			return h.Locks[len(h.Locks)-1], true
		}
	}
	return model.Lock{}, false
}

// waitFor polls cond for up to 15 seconds and fails the test naming what it
// waited for.
func waitFor(t *testing.T, host *liveHost, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-host.done:
			t.Fatalf("CLIProxyAPI exited waiting for %s", what)
		default:
		}
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
