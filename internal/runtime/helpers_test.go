package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// testNow is the fixed clock every unit test runs at; it matches the pace
// package's live-seat fixtures so those numbers can be reused verbatim.
var testNow = time.Date(2026, 9, 4, 22, 30, 0, 0, time.UTC)

func at(t *testing.T, ts string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("parse %q: %v", ts, err)
	}
	return parsed
}

// fakeHost answers host callbacks from canned data and records every call so
// a test can assert what left the plugin.
type fakeHost struct {
	mu      sync.Mutex
	calls   []hostCall
	files   []HostAuthFileEntry
	listErr error
	auths   map[string]json.RawMessage
	http    func(req HostHTTPRequest) (HostHTTPResponse, error)
	// httpGate, when non-nil, blocks host.http.do until closed.
	httpGate chan struct{}
	// logGate, when non-nil, blocks host.log until closed.
	logGate chan struct{}
}

type hostCall struct {
	method  string
	payload string
}

func newFakeHost() *fakeHost {
	return &fakeHost{auths: make(map[string]json.RawMessage)}
}

func (f *fakeHost) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, hostCall{method: method, payload: string(payload)})
	f.mu.Unlock()

	switch method {
	case MethodHostLog:
		if f.logGate != nil {
			<-f.logGate
		}
		return okEnvelope(emptyResult)
	case MethodHostAuthList:
		f.mu.Lock()
		files, err := f.files, f.listErr
		f.mu.Unlock()
		if err != nil {
			return errorEnvelope("auth_list_failed", err.Error()), nil
		}
		return okEnvelope(HostAuthListResponse{Files: files})
	case MethodHostAuthGet:
		var req HostAuthGetRequest
		_ = json.Unmarshal(payload, &req)
		f.mu.Lock()
		raw, ok := f.auths[req.AuthIndex]
		f.mu.Unlock()
		if !ok {
			return errorEnvelope("auth_not_found", "auth not found for auth_index "+req.AuthIndex), nil
		}
		return okEnvelope(HostAuthGetResponse{AuthIndex: req.AuthIndex, Name: req.AuthIndex + ".json", JSON: raw})
	case MethodHostHTTPDo:
		if f.httpGate != nil {
			<-f.httpGate
		}
		var req HostHTTPRequest
		_ = json.Unmarshal(payload, &req)
		if f.http == nil {
			return errorEnvelope("http_failed", "no upstream configured"), nil
		}
		resp, err := f.http(req)
		if err != nil {
			return errorEnvelope("http_failed", err.Error()), nil
		}
		return okEnvelope(resp)
	default:
		return nil, errors.New("unexpected host method " + method)
	}
}

// logs returns every host.log request in order.
func (f *fakeHost) logs() []HostLogRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]HostLogRequest, 0)
	for _, c := range f.calls {
		if c.method != MethodHostLog {
			continue
		}
		var req HostLogRequest
		_ = json.Unmarshal([]byte(c.payload), &req)
		out = append(out, req)
	}
	return out
}

// sent reports whether any payload to the host, or any log line, contains s.
func (f *fakeHost) sent(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c.payload, s) {
			return true
		}
	}
	return false
}

// methodsContaining lists, once each, the host methods whose payload contains
// s, so a test can assert where a value was allowed to travel.
func (f *fakeHost) methodsContaining(s string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, c := range f.calls {
		if _, dup := seen[c.method]; dup || !strings.Contains(c.payload, s) {
			continue
		}
		seen[c.method] = struct{}{}
		out = append(out, c.method)
	}
	return out
}

func (f *fakeHost) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == method {
			n++
		}
	}
	return n
}

// fakeBindings is an in-memory BindingStore with the same scoping rules as
// session.Store and no expiry, which keeps the scheduler tests about routing
// rather than about TTLs.
type fakeBindings struct {
	mu    sync.Mutex
	items map[string]model.Binding
	ttl   time.Duration
	max   int
}

func newFakeBindings(ttl time.Duration, max int) *fakeBindings {
	return &fakeBindings{items: make(map[string]model.Binding), ttl: ttl, max: max}
}

func bindKey(provider, modelID, key string) string { return provider + "|" + modelID + "|" + key }

func (f *fakeBindings) Lookup(provider, modelID, key string, now time.Time) (model.Binding, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.items[bindKey(provider, modelID, key)]
	if !ok {
		return model.Binding{}, false
	}
	b.LastSeen = now
	b.Hits++
	f.items[bindKey(provider, modelID, key)] = b
	return b, true
}

func (f *fakeBindings) Bind(provider, modelID, key, authID string, now time.Time) model.Binding {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := model.Binding{SessionKey: key, Provider: provider, Model: modelID, AuthID: authID, BoundAt: now, LastSeen: now}
	f.items[bindKey(provider, modelID, key)] = b
	return b
}

func (f *fakeBindings) Drop(provider, modelID, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, bindKey(provider, modelID, key))
}

func (f *fakeBindings) DropAuth(authID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, b := range f.items {
		if b.AuthID == authID {
			delete(f.items, k)
			n++
		}
	}
	return n
}

func (f *fakeBindings) All() []model.Binding {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.Binding, 0, len(f.items))
	for _, b := range f.items {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionKey < out[j].SessionKey })
	return out
}

func (f *fakeBindings) CountByAuth() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int)
	for _, b := range f.items {
		out[b.AuthID]++
	}
	return out
}

func (f *fakeBindings) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.items)
}

func (f *fakeBindings) Sweep(now time.Time) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, b := range f.items {
		if f.ttl > 0 && now.Sub(b.LastSeen) > f.ttl {
			delete(f.items, k)
			n++
		}
	}
	return n
}

// testPlugin is a registered plugin with fakes wired in.
type testPlugin struct {
	*Plugin
	host     *fakeHost
	bindings *fakeBindings
	// built counts binding-store constructions, to observe reconfigure.
	built int
}

const testConfigYAML = "enabled: true\npriority: 100\n"

// newTestPlugin registers a plugin at testNow with the given YAML block. The
// first poll is pushed out of reach so it cannot race a test's assertions.
func newTestPlugin(t *testing.T, configYAML string) *testPlugin {
	t.Helper()
	tp := &testPlugin{host: newFakeHost()}
	tp.Plugin = New(Options{
		Name:       "cpa-claude-quota-scheduler",
		Version:    "0.0.0-test",
		Author:     "yuya-iwabuchi",
		Repository: "https://example.invalid/repo",
		Host:       tp.host.call,
		Now:        func() time.Time { return testNow },
		// A real home directory must never be read or written by a test.
		HistoryFile: filepath.Join(t.TempDir(), "history.json"),
		NewBindingStore: func(ttl time.Duration, max int) BindingStore {
			tp.built++
			tp.bindings = newFakeBindings(ttl, max)
			return tp.bindings
		},
	})
	tp.startDelay = time.Hour
	t.Cleanup(tp.Shutdown)
	tp.register(t, MethodPluginRegister, configYAML)
	return tp
}

// register drives plugin.register or plugin.reconfigure and returns the
// decoded result.
func (tp *testPlugin) register(t *testing.T, method, configYAML string) RegisterResult {
	t.Helper()
	payload, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte(configYAML), SchemaVersion: 4})
	var out RegisterResult
	tp.callOK(t, method, payload, &out)
	return out
}

// callOK invokes a method, asserts a success envelope, and decodes the result.
func (tp *testPlugin) callOK(t *testing.T, method string, payload []byte, out any) {
	t.Helper()
	raw, ok := tp.Call(method, payload)
	if !ok {
		t.Fatalf("%s returned an error envelope: %s", method, raw)
	}
	if err := unwrapEnvelope(raw, out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// pick drives scheduler.pick with a struct and returns the decoded response.
func (tp *testPlugin) pick(t *testing.T, req SchedulerPickRequest) SchedulerPickResponse {
	t.Helper()
	payload, _ := json.Marshal(req)
	var out SchedulerPickResponse
	tp.callOK(t, MethodSchedulerPick, payload, &out)
	return out
}

// lastDecision is the most recent recorded decision.
func (tp *testPlugin) lastDecision(t *testing.T) model.Decision {
	t.Helper()
	all := tp.decisions.newestFirst()
	if len(all) == 0 {
		t.Fatal("no decision recorded")
	}
	return all[0]
}

// pickRequest builds a mixed-route pick for the claude provider, with the
// bridge key set when key is non-empty.
func pickRequest(modelID, key string, candidates ...string) SchedulerPickRequest {
	req := SchedulerPickRequest{
		Providers: []string{"claude"},
		Model:     modelID,
		Options: SchedulerOptions{
			Headers:  map[string][]string{"Anthropic-Version": {"2023-06-01"}},
			Metadata: map[string]any{MetadataSessionAffinityProvider: "mixed"},
		},
	}
	if key != "" {
		req.Options.Headers[HeaderSessionKey] = []string{key}
	}
	for _, id := range candidates {
		req.Candidates = append(req.Candidates, SchedulerAuthCandidate{
			ID: id, Provider: "claude", Status: "active",
			Attributes: map[string]string{"auth_kind": "oauth", "source_backend": "file"},
		})
	}
	return req
}

// Live-seat fixtures from the pace tests, at testNow: seat B is behind a
// weekly curve that resets in hours, seat A is ahead of one that just began,
// so for a Fable model seat B wins the cold pick.
const fableModel = "claude-fable-5-1"

func seatA(t *testing.T) model.AuthSnapshot {
	return seat("seat-a", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0, ResetsAt: at(t, "2026-09-05T03:09:59Z"), Duration: model.SessionDuration},
		model.Window{Kind: model.WindowWeekly, Utilization: 0.04, ResetsAt: at(t, "2026-09-11T18:59:59Z"), Duration: model.WeeklyDuration},
		model.Window{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0, ResetsAt: at(t, "2026-09-11T18:59:59Z"), Duration: model.WeeklyDuration},
	)
}

func seatB(t *testing.T) model.AuthSnapshot {
	return seat("seat-b", testNow,
		model.Window{Kind: model.WindowSession, Utilization: 0, ResetsAt: at(t, "2026-09-05T03:20:00Z"), Duration: model.SessionDuration},
		model.Window{Kind: model.WindowWeekly, Utilization: 0.54, ResetsAt: at(t, "2026-09-05T06:00:00Z"), Duration: model.WeeklyDuration},
		model.Window{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0.67, ResetsAt: at(t, "2026-09-05T06:00:00Z"), Duration: model.WeeklyDuration},
	)
}

func seat(id string, observed time.Time, windows ...model.Window) model.AuthSnapshot {
	return model.AuthSnapshot{AuthID: id, Windows: windows, ObservedAt: observed, Source: model.SourceUsageEndpoint}
}

// claudeCodeBody is a /v1/messages body as Claude Code sends it, with the
// session id in metadata.user_id.
func claudeCodeBody(sessionID string) []byte {
	return []byte(fmt.Sprintf(`{"model":%q,"max_tokens":16,"metadata":{"user_id":"user_abc_account_def_session_%s"},"messages":[{"role":"user","content":"ping"}]}`, fableModel, sessionID))
}

func interceptPayload(t *testing.T, body []byte) []byte {
	t.Helper()
	payload, err := json.Marshal(RequestInterceptRequest{
		RequestID: "req-1",
		Model:     fableModel,
		Headers:   http.Header{"Content-Type": {"application/json"}},
		Body:      body,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// bridgeKeyFor runs the interceptor on a body and returns the session key it
// derived, which is how a test obtains a key without reaching into session.
func (tp *testPlugin) bridgeKeyFor(t *testing.T, body []byte) string {
	t.Helper()
	var out RequestInterceptResponse
	tp.callOK(t, MethodRequestInterceptBefore, interceptPayload(t, body), &out)
	key := out.Headers.Get(HeaderSessionKey)
	if key == "" {
		t.Fatal("interceptor derived no session key")
	}
	return key
}
