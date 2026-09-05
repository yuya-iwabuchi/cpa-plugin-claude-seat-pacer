package runtime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

const (
	testMgmtBase     = "/v0/management"
	testResourceBase = "/v0/resource/plugins/cpa-claude-quota-scheduler"
	testMgmtPrefix   = testMgmtBase + "/plugins/cpa-claude-quota-scheduler"
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
		"POST " + testMgmtPrefix + "/refresh",
		"POST " + testMgmtPrefix + "/unbind",
		"POST " + testMgmtPrefix + "/bindings/sweep",
	} {
		if _, ok := routes[want]; !ok {
			t.Errorf("route %q not declared; got %v", want, out.Routes)
		}
	}
	if len(out.Resources) != 2 {
		t.Fatalf("resources = %+v, want index.html and api/status", out.Resources)
	}
	if out.Resources[0].Path != testResourceBase+"/index.html" || out.Resources[0].Menu != menuLabel {
		t.Errorf("index resource = %+v", out.Resources[0])
	}
	if out.Resources[1].Path != testResourceBase+"/api/status" || out.Resources[1].Menu != "" {
		t.Errorf("status resource = %+v, want no menu entry", out.Resources[1])
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

	for _, tc := range []struct{ path, want string }{
		{testResourceBase + "/index.html", "/index.html"},
		{testResourceBase + "/api/status", "/api/status"},
		{testResourceBase + "/", "/"},
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
