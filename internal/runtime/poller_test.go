package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/quota"
)

const secretToken = "sk-ant-oat01-SECRET-TOKEN-DO-NOT-LOG"

// usagePayload is a real usage-endpoint body from the quota fixtures.
func usagePayload(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "quota", "testdata", "usage_early_week.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// pollFixture wires two Claude OAuth files, one disabled Claude file, one
// Claude API-key record with no token, and one Gemini file into the fake host.
func pollFixture(t *testing.T, tp *testPlugin) {
	t.Helper()
	tp.host.files = []HostAuthFileEntry{
		{ID: "claude-a.json", AuthIndex: "idx-a", Name: "claude-a.json", Type: "claude", Provider: "claude", Email: "a@example.com", Status: "active", Priority: 3},
		{ID: "claude-b.json", AuthIndex: "idx-b", Name: "claude-b.json", Type: "claude", Provider: "claude", Label: "Seat B", Status: "active", Priority: 3},
		{ID: "claude-off.json", AuthIndex: "idx-off", Name: "claude-off.json", Type: "claude", Provider: "claude", Disabled: true, Status: "disabled"},
		{ID: "claude-key", AuthIndex: "idx-key", Name: "claude-key", Type: "claude", Provider: "claude", Status: "active"},
		{ID: "gemini.json", AuthIndex: "idx-g", Name: "gemini.json", Type: "gemini", Provider: "gemini", Status: "active"},
	}
	tp.host.auths["idx-a"] = json.RawMessage(`{"type":"claude","email":"a@example.com","access_token":"` + secretToken + `","refresh_token":"rt-a"}`)
	tp.host.auths["idx-b"] = json.RawMessage(`{"type":"claude","access_token":"` + secretToken + `-b","refresh_token":"rt-b"}`)
	tp.host.auths["idx-off"] = json.RawMessage(`{"type":"claude","access_token":"off"}`)
	tp.host.auths["idx-key"] = json.RawMessage(`{"type":"claude","api_key":"sk-ant-api03-KEY"}`)
	tp.host.auths["idx-g"] = json.RawMessage(`{"type":"gemini","access_token":"g"}`)
	body := usagePayload(t)
	tp.host.http = func(req HostHTTPRequest) (HostHTTPResponse, error) {
		if req.URL != model.DefaultUsageURL {
			return HostHTTPResponse{StatusCode: 404}, nil
		}
		if !strings.HasPrefix(req.Headers.Get("Authorization"), "Bearer "+secretToken) {
			return HostHTTPResponse{StatusCode: 401}, nil
		}
		return HostHTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
	}
}

func TestPollFetchesGovernedOAuthCredentials(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)

	if err := tp.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	snaps := tp.quota.All()
	if len(snaps) != 2 || snaps[0].AuthID != "claude-a.json" || snaps[1].AuthID != "claude-b.json" {
		ids := make([]string, 0, len(snaps))
		for _, s := range snaps {
			ids = append(ids, s.AuthID)
		}
		t.Fatalf("snapshots = %v, want exactly the two enabled Claude OAuth credentials", ids)
	}
	for _, s := range snaps {
		if len(s.Windows) == 0 || s.Err != "" || !s.ObservedAt.Equal(testNow) {
			t.Errorf("snapshot %s = err %q windows %d observed %v", s.AuthID, s.Err, len(s.Windows), s.ObservedAt)
		}
	}
	if snaps[0].Label != "a@example.com" || snaps[1].Label != "Seat B" {
		t.Errorf("labels = %q %q, want email then host label", snaps[0].Label, snaps[1].Label)
	}
	if tp.host.count(MethodHostHTTPDo) != 2 {
		t.Errorf("host.http.do called %d times, want 2", tp.host.count(MethodHostHTTPDo))
	}
	// The token travels only inside host.http.do's Authorization header.
	for _, method := range tp.host.methodsContaining("SECRET-TOKEN") {
		if method != MethodHostHTTPDo {
			t.Errorf("the access token reached %s", method)
		}
	}
	for _, l := range tp.host.logs() {
		if raw, _ := json.Marshal(l); strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "rt-a") {
			t.Errorf("credential material reached host.log: %s", raw)
		}
	}

	status := tp.Status(testNow, "")
	if len(status.Auths) != 4 {
		t.Fatalf("status rows = %d, want the four governed entries", len(status.Auths))
	}
	for _, row := range status.Auths {
		if row.Provider != "claude" {
			t.Errorf("row %s provider = %q", row.AuthID, row.Provider)
		}
	}
	if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "claude-key") {
		t.Errorf("warnings = %v, want only the API-key record flagged as having no snapshot", status.Warnings)
	}
}

func TestPollRecordsFailuresAndPrunesRemovedCredentials(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The endpoint starts refusing one credential and the other disappears.
	tp.host.mu.Lock()
	tp.host.files = tp.host.files[:1]
	tp.host.mu.Unlock()
	tp.host.http = func(HostHTTPRequest) (HostHTTPResponse, error) { return HostHTTPResponse{StatusCode: 429}, nil }
	// A refresh whose every usage fetch failed reports it, so an operator
	// debugging a dead poller does not read a green light.
	if err := tp.refresh(context.Background()); err == nil {
		t.Error("refresh reported success with every usage fetch failing")
	}

	snaps := tp.quota.All()
	if len(snaps) != 1 || snaps[0].AuthID != "claude-a.json" {
		t.Fatalf("snapshots after prune = %+v, want claude-a only", snaps)
	}
	if snaps[0].ErrCategory != string(quota.CategoryRateLimited) || len(snaps[0].Windows) == 0 {
		t.Errorf("snapshot = err %q category %q windows %d, want prior readings kept with the new error", snaps[0].Err, snaps[0].ErrCategory, len(snaps[0].Windows))
	}
	status := tp.Status(testNow, "")
	joined := strings.Join(status.Warnings, "\n")
	if !strings.Contains(joined, "quota poll failing for claude-a.json (rate-limited)") {
		t.Errorf("warnings = %v, want the poll failure with its category", status.Warnings)
	}

	// A failing auth.list is reported and keeps the previous state.
	tp.host.mu.Lock()
	tp.host.listErr = errors.New("core auth manager unavailable")
	tp.host.mu.Unlock()
	if err := tp.refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "auth manager") {
		t.Errorf("refresh error = %v, want the list failure", err)
	}
	if len(tp.quota.All()) != 1 {
		t.Error("a list failure pruned the store")
	}
}

func TestPollSkipsWhenDisabled(t *testing.T) {
	tp := newTestPlugin(t, "enabled: false\n")
	pollFixture(t, tp)
	_ = tp.refresh(context.Background())
	if tp.host.count(MethodHostAuthList) != 0 {
		t.Error("a disabled plugin polled the host")
	}
}

func TestDisabledPollClearsAStaleListFailure(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	tp.host.mu.Lock()
	tp.host.listErr = errors.New("core auth manager unavailable")
	tp.host.mu.Unlock()
	if err := tp.refresh(context.Background()); err == nil {
		t.Fatal("refresh hid the list failure")
	}

	tp.register(t, MethodPluginReconfigure, "enabled: false\n")
	if err := tp.refresh(context.Background()); err != nil {
		t.Errorf("refresh on a disabled plugin = %v, want the stale failure cleared", err)
	}
	if w := tp.Status(testNow, ""); len(w.Warnings) != 1 || !strings.Contains(w.Warnings[0], "disabled") {
		t.Errorf("warnings = %v, want only the disabled notice", w.Warnings)
	}
}

func TestPollCutShortKeepsThePreviousView(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(tp.Status(testNow, "").Auths)

	// A refresh whose deadline expires part-way must not publish the half-read
	// credential list.
	tp.host.httpGate = make(chan struct{})
	defer close(tp.host.httpGate)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tp.refresh(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("refresh error = %v, want the deadline", err)
	}
	if got := len(tp.Status(testNow, "").Auths); got != before {
		t.Errorf("status rows = %d, want the %d from the last complete poll", got, before)
	}
	if len(tp.quota.All()) != 2 {
		t.Errorf("snapshots = %d, want both kept", len(tp.quota.All()))
	}
}

func TestHostDoerHonoursContext(t *testing.T) {
	h := newFakeHost()
	h.httpGate = make(chan struct{})
	defer close(h.httpGate)
	d := hostDoer{h: newHost(h.call)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := d.Do(ctx, quota.Request{Method: "GET", URL: model.DefaultUsageURL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if time.Since(start) > time.Second {
		t.Error("Do waited on the host past the context deadline")
	}
}

func TestShutdownWaitsForAnAbandonedHostCall(t *testing.T) {
	h := newFakeHost()
	h.httpGate = make(chan struct{})
	hst := newHost(h.call)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (hostDoer{h: hst}).Do(ctx, quota.Request{Method: "GET", URL: model.DefaultUsageURL}); err == nil {
		t.Fatal("Do waited for the wedged host")
	}

	// The call is still parked in the host, so the drain does not return until
	// it does: after this point the host frees its callback table and unloads
	// the library.
	drained := make(chan struct{})
	go func() {
		hst.drain(2 * time.Second)
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("drain returned while a host call was still outstanding")
	case <-time.After(50 * time.Millisecond):
	}
	close(h.httpGate)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drain did not return after the host answered")
	}
}

func TestDrainGivesUpOnAHostThatNeverAnswers(t *testing.T) {
	h := newFakeHost()
	h.httpGate = make(chan struct{})
	defer close(h.httpGate)
	hst := newHost(h.call)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _ = (hostDoer{h: hst}).Do(ctx, quota.Request{Method: "GET", URL: model.DefaultUsageURL})

	start := time.Now()
	hst.drain(50 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("drain took %v, want it bounded by its deadline", elapsed)
	}
}

func TestHostDoerTranslatesResponse(t *testing.T) {
	h := newFakeHost()
	var seen HostHTTPRequest
	h.http = func(req HostHTTPRequest) (HostHTTPResponse, error) {
		seen = req
		return HostHTTPResponse{StatusCode: 200, Headers: http.Header{"X-Test": {"1"}}, Body: []byte(`{}`)}, nil
	}
	d := hostDoer{h: newHost(h.call)}
	resp, err := d.Do(context.Background(), quota.Request{Method: "GET", URL: "https://u", Header: map[string]string{"Authorization": "Bearer x"}})
	if err != nil || resp.StatusCode != 200 || resp.Header["X-Test"][0] != "1" || string(resp.Body) != "{}" {
		t.Errorf("resp = %+v err = %v", resp, err)
	}
	if seen.Method != "GET" || seen.URL != "https://u" || seen.Headers.Get("Authorization") != "Bearer x" {
		t.Errorf("host saw %+v", seen)
	}
}
