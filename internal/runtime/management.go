package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// menuLabel is the Management Center menu entry for the status page.
const menuLabel = "Quota Scheduler"

// Resource paths under the plugin's resource prefix. The host matches a
// resource route by exact path and rejects a bare "/" (the trailing slash is
// trimmed and an empty path refused, internal/pluginhost/management.go:209),
// so every page the app needs is its own route and the app must fetch
// "api/status" relative to index.html. The host serves these without the
// management key, so they carry read-only, credential-free content.
const (
	resourceIndexPath  = "/index.html"
	resourceStatusPath = "/api/status"
)

// Management routes, authenticated by the host with the management key.
// Setting Menu on a GET management route would reclassify it as an
// unauthenticated resource (management.go:153), so none of these carry one.
const (
	routeStatus  = "/status"
	routeRefresh = "/refresh"
	routeUnbind  = "/unbind"
	routeSweep   = "/bindings/sweep"
)

// Defaults for the prefixes when the registration request omits them; the host
// always sends both (internal/pluginhost/management.go:113).
const (
	defaultManagementBase = "/v0/management"
	defaultResourceBase   = "/v0/resource/plugins/"
)

// refreshHeadroom bounds a management-triggered refresh beyond the sum of
// per-credential fetch timeouts, covering the auth.list and auth.get calls.
const refreshHeadroom = 5 * time.Second

// managementRegister answers management.register. The host re-issues it on
// every reconfigure, so the same routes come back each time.
func (p *Plugin) managementRegister(payload []byte) ([]byte, error) {
	var req ManagementRegistrationRequest
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &req)
	}
	mgmtBase := strings.TrimRight(req.BasePath, "/")
	if mgmtBase == "" {
		mgmtBase = defaultManagementBase
	}
	resourceBase := strings.TrimRight(req.ResourceBasePath, "/")
	if resourceBase == "" {
		resourceBase = defaultResourceBase + p.opts.Name
	}
	p.mu.Lock()
	p.mgmtBase = mgmtBase
	p.resourceBase = resourceBase
	p.mu.Unlock()

	prefix := p.managementPrefix(mgmtBase)
	resp := ManagementRegistrationResponse{
		Routes: []ManagementRoute{
			{Method: http.MethodGet, Path: prefix + routeStatus, Description: "Quota, pace scores, bindings and routing decisions as JSON. Takes ?model=<id>."},
			{Method: http.MethodPost, Path: prefix + routeRefresh, Description: "Poll every governed credential's usage endpoint now."},
			{Method: http.MethodPost, Path: prefix + routeUnbind, Description: "Drop every session binding on one credential. Takes ?auth_id=<id>."},
			{Method: http.MethodPost, Path: prefix + routeSweep, Description: "Drop session bindings idle past the affinity TTL."},
		},
	}
	if p.config().Web.Enabled {
		resp.Resources = []ResourceRoute{
			{Path: resourceBase + resourceIndexPath, Menu: menuLabel, Description: "Quota scheduler status page."},
			{Path: resourceBase + resourceStatusPath, Description: "Status JSON for the page."},
		}
	}
	return okEnvelope(resp)
}

// managementPrefix is the authenticated route prefix for this plugin.
func (p *Plugin) managementPrefix(mgmtBase string) string {
	return mgmtBase + "/plugins/" + p.opts.Name
}

// managementHandle answers management.handle for both surfaces. Failures are
// HTTP statuses inside a success envelope: an error envelope would fail the
// request at the host with a 502 and, for hooks, feed into its credential
// cooldown decisions.
//
// The host HTML-escapes every string in a JSON body on the authenticated
// routes (management.go:267 via htmlsanitize), so & < > " arrive escaped
// there; resource-route bodies pass through untouched, which is why the
// status app reads its JSON from the resource route.
func (p *Plugin) managementHandle(payload []byte) ([]byte, error) {
	var req ManagementRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": "undecodable management request"}))
	}
	p.mu.Lock()
	mgmtBase, resourceBase := p.mgmtBase, p.resourceBase
	p.mu.Unlock()
	if mgmtBase == "" {
		mgmtBase = defaultManagementBase
	}
	if resourceBase == "" {
		resourceBase = defaultResourceBase + p.opts.Name
	}

	if strings.HasPrefix(req.Path, resourceBase+"/") {
		return okEnvelope(p.serveResource(req, strings.TrimPrefix(req.Path, resourceBase)))
	}

	prefix := p.managementPrefix(mgmtBase)
	if !strings.HasPrefix(req.Path, prefix+"/") {
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown path"}))
	}
	route := strings.TrimPrefix(req.Path, prefix)
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	want := map[string]string{routeStatus: http.MethodGet, routeRefresh: http.MethodPost, routeUnbind: http.MethodPost, routeSweep: http.MethodPost}[route]
	switch {
	case want == "":
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown path"}))
	case method != want:
		return okEnvelope(jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": want + " only"}))
	}

	now := p.now()
	switch route {
	case routeStatus:
		return okEnvelope(jsonResponse(http.StatusOK, p.Status(now, req.Query.Get("model"))))
	case routeRefresh:
		ctx, cancel := context.WithTimeout(context.Background(), p.refreshTimeout())
		defer cancel()
		result := map[string]any{"ok": true}
		if err := p.refresh(ctx); err != nil {
			result["ok"] = false
			result["error"] = err.Error()
		}
		result["auths"] = len(p.quota.All())
		return okEnvelope(jsonResponse(http.StatusOK, result))
	case routeUnbind:
		authID := strings.TrimSpace(req.Query.Get("auth_id"))
		if authID == "" {
			return okEnvelope(jsonResponse(http.StatusBadRequest, map[string]string{"error": "auth_id is required"}))
		}
		dropped := p.bindingStore().DropAuth(authID)
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"auth_id": authID, "dropped": dropped}))
	default:
		swept := p.bindingStore().Sweep(now)
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"swept": swept, "remaining": p.bindingStore().Len()}))
	}
}

// refreshTimeout bounds a manual refresh by the number of credentials it has
// to fetch.
func (p *Plugin) refreshTimeout() time.Duration {
	cfg := p.config()
	p.mu.Lock()
	n := len(p.auths)
	p.mu.Unlock()
	if n < 1 {
		n = 1
	}
	return time.Duration(n)*cfg.Quota.RequestTimeout + refreshHeadroom
}

// serveResource hands a resource request to the status app with the plugin
// prefix stripped, so the app sees /index.html and /api/status. Without an
// installed app the route answers 503 rather than 404, which tells an operator
// the route exists and the build is missing its front end. With web.enabled
// off the routes are never declared, so a request that still arrives — from a
// registration the host has not replaced yet — is a 404.
func (p *Plugin) serveResource(req ManagementRequest, path string) ManagementResponse {
	h := p.resource.Load()
	if h == nil {
		return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "status app unavailable in this build"})
	}
	if !p.config().Web.Enabled {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown path"})
	}
	return serveThrough(h.h, req, path)
}

// serveThrough replays a host management request against an http.Handler and
// captures the response.
func serveThrough(h http.Handler, req ManagementRequest, path string) ManagementResponse {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	target := path
	if len(req.Query) > 0 {
		target += "?" + req.Query.Encode()
	}
	r := httptest.NewRequest(method, target, bytes.NewReader(req.Body))
	for name, values := range req.Headers {
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return ManagementResponse{
		StatusCode: rec.Code,
		Headers:    rec.Header().Clone(),
		Body:       rec.Body.Bytes(),
	}
}

// jsonResponse encodes a JSON management response. A body that cannot encode
// becomes a 500 with a fixed message rather than a failed call.
func jsonResponse(status int, body any) ManagementResponse {
	encoded, err := json.Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		encoded = []byte(`{"error":"failed to encode response"}`)
	}
	return ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: encoded,
	}
}
