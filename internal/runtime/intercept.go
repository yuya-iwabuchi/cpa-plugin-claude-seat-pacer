package runtime

import (
	"encoding/json"
	"net/http"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/session"
)

// Bridge headers. request.intercept_before derives the conversation identity
// from the request body and writes it into these headers; scheduler.pick, which
// never sees the body, reads them back from SchedulerOptions.Headers.
//
// The names are already in http.Header canonical form, so the host's merge
// does not rewrite them. They do not reach Anthropic: the Claude executor
// builds its upstream request from scratch and copies caller headers only from
// an allowlist — accept, accept-encoding, user-agent, x-app, x-client-app,
// x-client-request-id, x-anthropic-additional-protection and the anthropic-,
// x-stainless-, x-claude-code- and x-claude-remote- prefixes
// (internal/runtime/executor/claude_executor_request.go:680-701, :935-945).
// The X-Cqs- prefix matches none of those, so renaming a bridge header onto
// one of the forwarded prefixes would leak it upstream.
const (
	HeaderSessionKey    = "X-Cqs-Session-Key"
	HeaderSessionParent = "X-Cqs-Session-Parent"
	HeaderSubagent      = "X-Cqs-Subagent"
)

// interceptBefore answers request.intercept_before. A request with no
// derivable identity gets an empty response, never an error: the host feeds a
// plugin error's HTTPStatus into its credential cooldown logic, and an
// identity miss is not a request failure.
//
// The body is read for identity extraction only and is never logged.
func (p *Plugin) interceptBefore(payload []byte) ([]byte, error) {
	var req RequestInterceptRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return okEnvelope(RequestInterceptResponse{})
	}
	id := session.Extract(req.Headers, req.Body)
	if id.Key == "" {
		return okEnvelope(RequestInterceptResponse{})
	}
	headers := http.Header{HeaderSessionKey: {id.Key}}
	if id.Subagent {
		headers.Set(HeaderSubagent, "1")
		if id.ParentKey != "" {
			headers.Set(HeaderSessionParent, id.ParentKey)
		}
	}
	return okEnvelope(RequestInterceptResponse{Headers: headers})
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
