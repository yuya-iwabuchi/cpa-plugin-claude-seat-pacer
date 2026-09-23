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
const menuLabel = "Claude Seat Pacer"

// resourceIndexPath is the one resource route: the status page itself. The host
// serves resource routes to anyone who can reach its port, with no management
// key and no loopback check, so the route carries the static page and no data.
// The host matches a resource route by exact path and rejects a bare "/" (the
// trailing slash is trimmed and an empty path refused,
// internal/pluginhost/management.go:209).
const resourceIndexPath = "/index.html"

// appStatusPath is where the status app answers for its data inside the
// plugin. No resource route reaches it: routePageStatus does, behind the key.
const appStatusPath = "/api/status"

// Management routes, authenticated by the host with the management key and,
// unless remote-management.allow-remote is set, answered for loopback clients
// only. Setting Menu on a GET management route would reclassify it as an
// unauthenticated resource (management.go:153), so none of these carry one.
//
// routePageStatus is the status page's own data: the routeStatus payload as the
// status app shapes it, with non-finite readings made null, the binding list
// bounded and each seat named without its account address.
const (
	routeStatus     = "/status"
	routePageStatus = "/page-status"
	routeRefresh    = "/refresh"
	routeUnbind     = "/unbind"
	routeSweep      = "/bindings/sweep"
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

// pollBudget bounds one background poll. It is deliberately independent of the
// credential count: on the first poll after a load that count is still zero, and
// a budget sized from it cuts short the very read that would populate it, which
// stores an error snapshot carrying no window for every credential.
// quota.Client already caps each fetch at Quota.RequestTimeout, so this only
// has to outlast a whole poll and stop a wedged host callback from parking the
// loop for good.
const pollBudget = 2 * time.Minute

// MinForcedPollGap floors how often the page's status route may force a usage
// read. Every open page can ask for one, and without a floor a few tabs clicking
// Sync now could drive the usage endpoint fast enough to earn the pool a
// throttle. A forced read is otherwise the same work the loop does on its own.
const MinForcedPollGap = 10 * time.Second

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
			{Method: http.MethodGet, Path: prefix + routeStatus, Description: "Quota windows, pace scores, bindings and routing decisions as JSON. Takes ?model=<id>."},
			{Method: http.MethodPost, Path: prefix + routeRefresh, Description: "Poll every governed credential's usage endpoint now."},
			{Method: http.MethodPost, Path: prefix + routeUnbind, Description: "Drop every session binding on one credential. Takes ?auth_id=<id>."},
			{Method: http.MethodPost, Path: prefix + routeSweep, Description: "Drop session bindings idle past the affinity TTL."},
		},
	}
	// The page and its data route come and go together with web.enabled.
	if p.config().Web.Enabled {
		resp.Routes = append(resp.Routes, ManagementRoute{Method: http.MethodGet, Path: prefix + routePageStatus, Description: "The status page's data. Takes ?model=<id>, and ?sync=1 to read usage first."})
		resp.Resources = []ResourceRoute{
			{Path: resourceBase + resourceIndexPath, Menu: menuLabel, Description: "Status page. Its data comes from " + prefix + routePageStatus + ", which needs the management key."},
		}
	}
	return okEnvelope(resp)
}

// managementPrefix is the authenticated route prefix for this plugin.
func (p *Plugin) managementPrefix(mgmtBase string) string {
	return mgmtBase + "/plugins/" + p.opts.Name
}

// managementHandle answers management.handle for both surfaces. Failures are
// HTTP statuses inside a success envelope, because an error envelope would
// fail the request at the host with a 502.
//
// The host HTML-escapes every string value in a JSON body on the authenticated
// routes (htmlsanitize.JSONBody, html.EscapeString) for a plugin declaring a
// schema version below 6, so & < > " ' arrive as entities. The status page
// decodes them once on arrival; it renders with textContent, never as markup.
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
		path := strings.TrimPrefix(req.Path, resourceBase)
		if path != resourceIndexPath {
			return okEnvelope(jsonResponse(http.StatusNotFound, map[string]string{"error": "unknown path"}))
		}
		return okEnvelope(p.serveResource(req, path))
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
	want := map[string]string{routeStatus: http.MethodGet, routePageStatus: http.MethodGet, routeRefresh: http.MethodPost, routeUnbind: http.MethodPost, routeSweep: http.MethodPost}[route]
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
	case routePageStatus:
		return okEnvelope(p.serveResource(req, appStatusPath))
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
// to fetch. A manual refresh answers an HTTP request that waits on it, so it
// stays tight; the background loop uses pollBudget instead.
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

// serveResource hands a request to the status app at an app-relative path:
// /index.html for the resource route, /api/status for routePageStatus. Without
// an installed app it answers 503 rather than 404, whatever web.enabled says,
// which tells an operator the build is missing its front end. With an app and
// web.enabled off, neither path is declared and both answer 404, including a
// request that arrives from a registration the host has not replaced yet.
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

// SyncNow re-reads every governed credential's usage unless the last poll is
// more recent than MinForcedPollGap, and reports whether it read. It runs
// inline on the caller's goroutine, so a status response built after it
// carries the fresh reading.
func (p *Plugin) SyncNow(ctx context.Context) bool {
	// The slot is claimed under the lock before the poll runs, so concurrent
	// callers inside one window share a single read rather than each running
	// a full poll before the first has stamped polledAt. Only the read time
	// moves: the loop's timer is untouched by a forced read, so nextPollAt
	// still names the wake it will actually take.
	p.mu.Lock()
	now := p.now()
	if !p.polledAt.IsZero() && now.Sub(p.polledAt) < MinForcedPollGap {
		p.mu.Unlock()
		return false
	}
	p.polledAt = now
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, p.refreshTimeout())
	defer cancel()
	_ = p.refresh(ctx)
	return true
}
