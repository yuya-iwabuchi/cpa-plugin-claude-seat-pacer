package runtime

import (
	"encoding/json"
	"net/http"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/session"
)

// Bridge headers. request.intercept_before derives the conversation identity
// from the request headers or body and writes it into these headers;
// scheduler.pick, which never sees the body, reads them back from
// SchedulerOptions.Headers.
//
// The names are already in http.Header canonical form, so the host's merge
// does not rewrite them. They do not reach Anthropic: the Claude executor
// builds its upstream request from scratch and copies caller headers only from
// an allowlist — accept, accept-encoding, user-agent, x-app, x-client-app,
// x-client-request-id, x-anthropic-additional-protection and the anthropic-,
// x-stainless-, x-claude-code- and x-claude-remote- prefixes
// (CLIProxyAPI internal/runtime/executor/claude_executor_request.go:680-701,
// :935-945). The X-Cqs- prefix matches none of those, so renaming a bridge
// header onto one of the forwarded prefixes would leak it upstream.
const (
	HeaderSessionKey    = "X-Cqs-Session-Key"
	HeaderSessionParent = "X-Cqs-Session-Parent"
	HeaderSubagent      = "X-Cqs-Subagent"
)

// bridgeHeaders is the set the plugin owns end to end. The host merges an
// interceptor's headers over the client's own inbound headers rather than
// replacing them, so a response that only sets a header leaves any
// client-supplied value of the others intact: every response clears all three
// and then re-adds only what the derived identity justifies. Without that a
// client pins its own routing, or names another conversation's key and joins
// its credential.
var bridgeHeaders = []string{HeaderSessionKey, HeaderSessionParent, HeaderSubagent}

// interceptBefore answers request.intercept_before. A request with no
// derivable identity gets the bare clear, never an error: the host feeds a
// plugin error's HTTPStatus into its credential cooldown logic, and an
// identity miss is not a request failure.
//
// The body is read for identity extraction only and is never logged.
func (p *Plugin) interceptBefore(payload []byte) ([]byte, error) {
	var req RequestInterceptRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return okEnvelope(clearBridge())
	}
	id := session.Extract(req.Headers, req.Body)
	if id.Key == "" {
		return okEnvelope(clearBridge())
	}
	headers := http.Header{HeaderSessionKey: {id.Key}}
	if id.Subagent {
		headers.Set(HeaderSubagent, "1")
		if id.ParentKey != "" {
			headers.Set(HeaderSessionParent, id.ParentKey)
		}
	}
	resp := clearBridge()
	resp.Headers = headers
	return okEnvelope(resp)
}

// clearBridge is the interceptor response that carries no identity: it strips
// every bridge header the client may have sent and adds none. The host applies
// ClearHeaders before Headers, so a caller may fill Headers in on top of it.
func clearBridge() RequestInterceptResponse {
	return RequestInterceptResponse{ClearHeaders: bridgeHeaders}
}

// bridgeIdentity reads the bridge headers back at the pick.
type bridgeIdentity struct {
	key      string
	parent   string
	subagent bool
}

func readBridge(headers map[string][]string) bridgeIdentity {
	return bridgeIdentity{
		key:      headerValue(headers, HeaderSessionKey),
		parent:   headerValue(headers, HeaderSessionParent),
		subagent: headerValue(headers, HeaderSubagent) == "1",
	}
}
