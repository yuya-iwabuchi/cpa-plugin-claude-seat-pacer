package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/web"
)

const (
	testMgmtBase     = "/v0/management"
	testResourceBase = "/v0/resource/plugins/claude-seat-pacer"
	testMgmtPrefix   = testMgmtBase + "/plugins/claude-seat-pacer"
)

func (tp *testPlugin) registerManagement(t *testing.T) ManagementRegistrationResponse {
	t.Helper()
	payload := mustJSON(t, ManagementRegistrationRequest{BasePath: testMgmtBase, ResourceBasePath: testResourceBase})
	var out ManagementRegistrationResponse
	tp.callOK(t, MethodManagementRegister, payload, &out)
	return out
}

func (tp *testPlugin) manage(t *testing.T, method, path string, query url.Values) ManagementResponse {
	t.Helper()
	payload := mustJSON(t, ManagementRequest{Method: method, Path: path, Query: query, Headers: http.Header{"Authorization": {"Bearer key"}}})
	var out ManagementResponse
	tp.callOK(t, MethodManagementHandle, payload, &out)
	return out
}

func TestManagementRegisterDeclaresRoutesAndResources(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	out := tp.registerManagement(t)

	routes := make(map[string]ManagementRoute)
	for _, r := range out.Routes {
		if r.Menu != "" {
			t.Errorf("management route %s carries Menu %q, which the host would reclassify as an unauthenticated resource", r.Path, r.Menu)
		}
		routes[r.Method+" "+r.Path] = r
	}
	for _, want := range []string{
		"GET " + testMgmtPrefix + "/status",
		"GET " + testMgmtPrefix + "/page-status",
		"POST " + testMgmtPrefix + "/refresh",
		"POST " + testMgmtPrefix + "/unbind",
		"POST " + testMgmtPrefix + "/bindings/sweep",
	} {
		if _, ok := routes[want]; !ok {
			t.Errorf("route %q not declared; got %v", want, out.Routes)
		}
	}
	// The page is the only resource: the host serves resource routes with no
	// key, so the data the page renders is a management route.
	if len(out.Resources) != 1 {
		t.Fatalf("resources = %+v, want index.html alone", out.Resources)
	}
	if out.Resources[0].Path != testResourceBase+"/index.html" || out.Resources[0].Menu != menuLabel {
		t.Errorf("index resource = %+v", out.Resources[0])
	}

	// The same answer comes back on every registration.
	again := tp.registerManagement(t)
	if mustString(t, again) != mustString(t, out) {
		t.Error("management.register is not stable across calls")
	}

	// web.enabled false drops the resource routes and keeps the API.
	tp2 := newTestPlugin(t, testConfigYAML+"web:\n  enabled: false\n")
	if out := tp2.registerManagement(t); len(out.Resources) != 0 || len(out.Routes) != 4 {
		t.Errorf("web disabled: %+v", out)
	}
}

// recordingHandler stands in for the status app and reports what it was asked.
type recordingHandler struct {
	path, query, method, body, header string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.path, h.query, h.method, h.header = r.URL.Path, r.URL.RawQuery, r.Method, r.Header.Get("X-Probe")
	raw := make([]byte, 64)
	n, _ := r.Body.Read(raw)
	h.body = string(raw[:n])
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusTeapot)
	_, _ = w.Write([]byte("served " + r.URL.Path))
}

func TestManagementAdapterStripsResourcePrefix(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.registerManagement(t)
	h := &recordingHandler{}
	tp.SetResourceHandler(h)

	// A path that reaches no resource route never reaches the app, whatever
	// the app would answer for it: api/status is data, and the host refuses a
	// bare "/" before it gets here.
	for _, path := range []string{testResourceBase + "/api/status", testResourceBase + "/"} {
		h.path = ""
		var out ManagementResponse
		tp.callOK(t, MethodManagementHandle, mustJSON(t, ManagementRequest{Method: http.MethodGet, Path: path}), &out)
		if out.StatusCode != http.StatusNotFound || h.path != "" {
			t.Errorf("%s: status %d, handler saw %q; want 404 without reaching the app", path, out.StatusCode, h.path)
		}
	}

	for _, tc := range []struct{ path, want string }{
		{testResourceBase + "/index.html", "/index.html"},
		{testMgmtPrefix + "/page-status", "/api/status"},
	} {
		payload := mustJSON(t, ManagementRequest{
			Method:  http.MethodGet,
			Path:    tc.path,
			Query:   url.Values{"model": {"claude-opus-5"}},
			Headers: http.Header{"X-Probe": {"yes"}},
			Body:    []byte("ignored-by-get"),
		})
		var out ManagementResponse
		tp.callOK(t, MethodManagementHandle, payload, &out)
		if h.path != tc.want {
			t.Errorf("%s: handler saw path %q, want %q", tc.path, h.path, tc.want)
		}
		if h.query != "model=claude-opus-5" || h.method != http.MethodGet || h.header != "yes" || h.body != "ignored-by-get" {
			t.Errorf("%s: handler saw query=%q method=%q header=%q body=%q", tc.path, h.query, h.method, h.header, h.body)
		}
		if out.StatusCode != http.StatusTeapot || string(out.Body) != "served "+tc.want || out.Headers.Get("Content-Type") != "text/plain" {
			t.Errorf("%s: response = %d %q %v", tc.path, out.StatusCode, out.Body, out.Headers)
		}
	}

	// Without an app the resource route answers 503, not an error envelope.
	tp.SetResourceHandler(nil)
	resp := tp.manage(t, http.MethodGet, testResourceBase+"/index.html", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no handler: status %d, want 503", resp.StatusCode)
	}
}

func TestManagementStatusRoute(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.registerManagement(t)
	tp.seats(t)
	tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))

	resp := tp.manage(t, http.MethodGet, testMgmtPrefix+"/status", url.Values{"model": {"claude-opus-5"}})
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Headers.Get("Content-Type"), "application/json") {
		t.Fatalf("status = %d %v", resp.StatusCode, resp.Headers)
	}
	var status model.Status
	if err := json.Unmarshal(resp.Body, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Model != "claude-opus-5" || len(status.Decisions) != 1 || len(status.Bindings) != 1 {
		t.Errorf("status = model %q decisions %d bindings %d", status.Model, len(status.Decisions), len(status.Bindings))
	}
	if resp := tp.manage(t, http.MethodPost, testMgmtPrefix+"/status", nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp.StatusCode)
	}
	if resp := tp.manage(t, http.MethodGet, testMgmtPrefix+"/nope", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404", resp.StatusCode)
	}
	if resp := tp.manage(t, http.MethodGet, "/v0/management/other/status", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("foreign path = %d, want 404", resp.StatusCode)
	}
}

func TestManagementBindingRoutes(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML+"affinity:\n  ttl: 10m\n")
	tp.registerManagement(t)
	tp.seats(t)
	tp.pick(t, pickRequest(fableModel, "k1", "seat-a", "seat-b"))
	tp.pick(t, pickRequest(fableModel, "k2", "seat-a", "seat-b"))
	tp.bindings.Bind("claude", fableModel, "old", "seat-a", testNow.Add(-time.Hour))

	resp := tp.manage(t, http.MethodPost, testMgmtPrefix+"/unbind", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unbind without auth_id = %d, want 400", resp.StatusCode)
	}
	resp = tp.manage(t, http.MethodPost, testMgmtPrefix+"/unbind", url.Values{"auth_id": {"seat-b"}})
	var unbind struct {
		AuthID  string `json:"auth_id"`
		Dropped int    `json:"dropped"`
	}
	_ = json.Unmarshal(resp.Body, &unbind)
	if resp.StatusCode != http.StatusOK || unbind.Dropped != 2 || unbind.AuthID != "seat-b" {
		t.Errorf("unbind = %d %s", resp.StatusCode, resp.Body)
	}

	resp = tp.manage(t, http.MethodPost, testMgmtPrefix+"/bindings/sweep", nil)
	var sweep struct {
		Swept     int `json:"swept"`
		Remaining int `json:"remaining"`
	}
	_ = json.Unmarshal(resp.Body, &sweep)
	if resp.StatusCode != http.StatusOK || sweep.Swept != 1 || sweep.Remaining != 0 {
		t.Errorf("sweep = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestManagementRefreshRoute(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.registerManagement(t)
	pollFixture(t, tp)

	resp := tp.manage(t, http.MethodPost, testMgmtPrefix+"/refresh", nil)
	var out struct {
		OK    bool `json:"ok"`
		Auths int  `json:"auths"`
	}
	_ = json.Unmarshal(resp.Body, &out)
	if resp.StatusCode != http.StatusOK || !out.OK || out.Auths != 2 {
		t.Errorf("refresh = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestManagementHandleToleratesGarbage(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	raw, ok := tp.Call(MethodManagementHandle, []byte("garbage"))
	if !ok {
		t.Fatalf("garbage produced an error envelope: %s", raw)
	}
	var out ManagementResponse
	_ = unwrapEnvelope(raw, &out)
	if out.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", out.StatusCode)
	}
}

func mustString(t *testing.T, v any) string {
	t.Helper()
	return string(mustJSON(t, v))
}

// TestManagementStatusKeepsRealIDs covers the split between the two routes the
// same status reaches: the page's route names each seat by a hashed credential
// id so the page is safe to show, and the status route serves the id an
// operator needs for unbind.
func TestManagementStatusKeepsRealIDs(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.registerManagement(t)
	tp.SetResourceHandler(web.NewHandler(tp.Plugin))
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	tp.pick(t, pickRequest(fableModel, "k1", "claude-a.json", "claude-b.json"))

	managed := tp.manage(t, http.MethodGet, testMgmtPrefix+routeStatus, nil)
	if managed.StatusCode != http.StatusOK {
		t.Fatalf("management status = %d: %s", managed.StatusCode, managed.Body)
	}
	var full model.Status
	if err := json.Unmarshal(managed.Body, &full); err != nil {
		t.Fatalf("decode management status: %v", err)
	}
	real := make(map[string]bool)
	for _, row := range full.Auths {
		real[row.AuthID] = true
	}
	for _, id := range []string{"claude-a.json", "claude-b.json"} {
		if !real[id] {
			t.Errorf("management status lost the real id %q: %v", id, full.Auths)
		}
	}
	if len(full.Bindings) == 0 || !real[full.Bindings[0].AuthID] {
		t.Errorf("management bindings = %+v, want a real id", full.Bindings)
	}

	served := tp.manage(t, http.MethodGet, testMgmtPrefix+routePageStatus, nil)
	if served.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d: %s", served.StatusCode, served.Body)
	}
	// Matched as an id rather than as a substring: a credential the host
	// gives neither a label nor an email keeps its file name as its label,
	// and that name is the real id.
	for id := range real {
		if quoted := `auth_id":"` + id + `"`; strings.Contains(string(served.Body), quoted) {
			t.Errorf("the page's route serves the real id %q: %s", id, served.Body)
		}
	}
	var public model.Status
	if err := json.Unmarshal(served.Body, &public); err != nil {
		t.Fatalf("decode page status: %v", err)
	}
	if len(public.Auths) != len(full.Auths) || len(public.Bindings) != len(full.Bindings) {
		t.Errorf("resource view = %d auths %d bindings, want the same rows as %d and %d",
			len(public.Auths), len(public.Bindings), len(full.Auths), len(full.Bindings))
	}
	// The join the page makes: every id it correlates on has a row.
	rows := make(map[string]bool)
	for _, row := range public.Auths {
		rows[row.AuthID] = true
	}
	for _, b := range public.Bindings {
		if !rows[b.AuthID] {
			t.Errorf("binding on %q has no credential row: %v", b.AuthID, public.Auths)
		}
	}
	for _, d := range public.Decisions {
		if d.ChosenAuthID != "" && !rows[d.ChosenAuthID] {
			t.Errorf("decision chose %q, which has no credential row: %v", d.ChosenAuthID, public.Auths)
		}
	}
}

// A forced read is throttled, and it moves only the read time: the loop's
// timer is untouched, so the schedule the page counts down to stays honest.
func TestSyncNowIsThrottledAndLeavesTheScheduleAlone(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)

	if !tp.SyncNow(context.Background()) {
		t.Fatal("the first forced read was refused with no prior poll")
	}
	scheduled := testNow.Add(90 * time.Second)
	tp.mu.Lock()
	tp.nextPollAt = scheduled
	tp.mu.Unlock()

	if tp.SyncNow(context.Background()) {
		t.Error("a second forced read ran inside the throttle window")
	}
	tp.mu.Lock()
	next := tp.nextPollAt
	tp.mu.Unlock()
	if !next.Equal(scheduled) {
		t.Errorf("next poll = %v, want the loop's own schedule %v", next, scheduled)
	}

	// Past the floor it reads again.
	tp.mu.Lock()
	tp.polledAt = tp.polledAt.Add(-MinForcedPollGap - time.Second)
	tp.mu.Unlock()
	if !tp.SyncNow(context.Background()) {
		t.Error("a forced read past the throttle window was refused")
	}
}

// TestConcurrentSyncNowRunsOnePoll covers the throttle on the unauthenticated
// sync route: callers that arrive inside one window share a single read, so a
// burst cannot multiply the usage endpoint's traffic by its own size.
func TestConcurrentSyncNowRunsOnePoll(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	before := tp.host.count(MethodHostHTTPDo)

	// Every caller sits on the gate until all have called SyncNow, so the
	// throttle decision is made by all of them before any poll completes.
	tp.host.httpGate = make(chan struct{})
	const callers = 8
	var wg sync.WaitGroup
	ran := make(chan bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ran <- tp.SyncNow(context.Background())
		}()
	}
	// Let the goroutines reach the throttle check, then release the poll.
	time.Sleep(50 * time.Millisecond)
	close(tp.host.httpGate)
	wg.Wait()
	close(ran)

	polls := 0
	for r := range ran {
		if r {
			polls++
		}
	}
	if polls != 1 {
		t.Errorf("%d of %d concurrent forced reads ran a poll, want exactly 1", polls, callers)
	}
	perPoll := tp.host.count(MethodHostHTTPDo) - before
	if perPoll > 2 {
		t.Errorf("host.http.do was called %d times for one forced read of two seats", perPoll)
	}
}

// TestPageStatusFollowsTheApp covers the page's data route when the page is
// not being served: with web.enabled off it answers 404, and with no app
// installed it answers 503, whatever web.enabled says.
func TestPageStatusFollowsTheApp(t *testing.T) {
	off := newTestPlugin(t, testConfigYAML+"web:\n  enabled: false\n")
	off.registerManagement(t)
	off.SetResourceHandler(&recordingHandler{})
	if got := off.manage(t, http.MethodGet, testMgmtPrefix+routePageStatus, nil); got.StatusCode != http.StatusNotFound {
		t.Errorf("web disabled: page-status = %d %s, want 404", got.StatusCode, got.Body)
	}

	bare := newTestPlugin(t, testConfigYAML)
	bare.registerManagement(t)
	bare.SetResourceHandler(nil)
	if got := bare.manage(t, http.MethodGet, testMgmtPrefix+routePageStatus, nil); got.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no app: page-status = %d %s, want 503", got.StatusCode, got.Body)
	}
}
