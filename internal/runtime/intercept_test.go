package runtime

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var hexKey = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestInterceptBeforeInjectsBridgeHeadersForClaudeCodeBody(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)

	var out RequestInterceptResponse
	tp.callOK(t, MethodRequestInterceptBefore, interceptPayload(t, claudeCodeBody("11111111-1111-1111-1111-111111111111")), &out)

	key := out.Headers.Get(HeaderSessionKey)
	if !hexKey.MatchString(key) {
		t.Fatalf("%s = %q, want a 32-hex hashed key", HeaderSessionKey, key)
	}
	if out.Headers.Get(HeaderSubagent) != "" || out.Headers.Get(HeaderSessionParent) != "" {
		t.Errorf("root session carried subagent headers: %v", out.Headers)
	}
	if len(out.Body) != 0 || out.Terminate {
		t.Errorf("interceptor modified the request beyond headers: %+v", out)
	}
	assertClearsBridge(t, out)

	// The same session id yields the same key on every request.
	if again := tp.bridgeKeyFor(t, claudeCodeBody("11111111-1111-1111-1111-111111111111")); again != key {
		t.Errorf("key changed between requests: %q then %q", key, again)
	}
	if other := tp.bridgeKeyFor(t, claudeCodeBody("22222222-2222-2222-2222-222222222222")); other == key {
		t.Error("different sessions produced the same key")
	}
	// The body itself never travels to the host's log.
	if tp.host.sent("ping") || tp.host.sent("user_abc_account") {
		t.Error("request body content reached a host callback")
	}
}

func TestInterceptBeforeMarksSubagents(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	body := []byte(`{"model":"claude-opus-5","metadata":{"user_id":{"session_id":"child-1","parent_session_id":"parent-1","agent_id":"agent-7"}},"messages":[]}`)

	var out RequestInterceptResponse
	tp.callOK(t, MethodRequestInterceptBefore, interceptPayload(t, body), &out)
	if out.Headers.Get(HeaderSubagent) != "1" {
		t.Errorf("%s = %q, want 1", HeaderSubagent, out.Headers.Get(HeaderSubagent))
	}
	parent := out.Headers.Get(HeaderSessionParent)
	if !hexKey.MatchString(parent) || parent == out.Headers.Get(HeaderSessionKey) {
		t.Errorf("%s = %q, want the parent's distinct hashed key", HeaderSessionParent, parent)
	}
	// The parent's own requests derive the key the child points at.
	parentBody := []byte(`{"model":"claude-opus-5","metadata":{"user_id":{"session_id":"parent-1"}},"messages":[]}`)
	if got := tp.bridgeKeyFor(t, parentBody); got != parent {
		t.Errorf("parent key = %q, child's parent header = %q", got, parent)
	}
}

func TestInterceptBeforeSetsNoHeadersWithoutIdentity(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	for name, body := range map[string][]byte{
		"no ids and no content": []byte(`{"model":"claude-opus-5","messages":[]}`),
		"empty body":            nil,
		"not json":              []byte("<html>"),
	} {
		t.Run(name, func(t *testing.T) {
			var out RequestInterceptResponse
			tp.callOK(t, MethodRequestInterceptBefore, interceptPayload(t, body), &out)
			if len(out.Headers) != 0 || len(out.Body) != 0 || out.Terminate {
				t.Errorf("identity miss modified the request: %+v", out)
			}
			assertClearsBridge(t, out)
		})
	}
}

func TestInterceptBeforeToleratesUndecodablePayload(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	var out RequestInterceptResponse
	tp.callOK(t, MethodRequestInterceptBefore, []byte("garbage"), &out)
	assertClearsBridge(t, out)
}

// TestInterceptBeforeDoesNotLetAClientSetTheBridgeHeaders is the security
// property the bridge rests on: the host merges the plugin's headers over the
// client's own inbound headers, so a response that did not clear these would
// let a client pin its own routing or join another conversation's credential.
func TestInterceptBeforeDoesNotLetAClientSetTheBridgeHeaders(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	spoofed := http.Header{
		"Content-Type":      {"application/json"},
		HeaderSessionKey:    {"victim-key"},
		HeaderSessionParent: {"victim-parent"},
		HeaderSubagent:      {"1"},
	}

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"identity derived", claudeCodeBody("11111111-1111-1111-1111-111111111111")},
		{"no identity", []byte(`{"model":"claude-opus-5","messages":[]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := mustJSON(t, RequestInterceptRequest{
				RequestID: "req-1", Model: fableModel, Headers: spoofed, Body: tc.body,
			})
			var out RequestInterceptResponse
			tp.callOK(t, MethodRequestInterceptBefore, payload, &out)
			assertClearsBridge(t, out)
			for _, name := range []string{HeaderSessionKey, HeaderSessionParent, HeaderSubagent} {
				if got := out.Headers.Get(name); strings.HasPrefix(got, "victim") {
					t.Errorf("%s = %q, want the client's value replaced or dropped", name, got)
				}
			}
			// A root session is never marked as somebody's child.
			if out.Headers.Get(HeaderSubagent) != "" || out.Headers.Get(HeaderSessionParent) != "" {
				t.Errorf("headers = %v, want no subagent pin from a client claim", out.Headers)
			}
		})
	}
}

// assertClearsBridge checks that a response removes every bridge header before
// setting the ones the plugin owns.
func assertClearsBridge(t *testing.T, resp RequestInterceptResponse) {
	t.Helper()
	for _, name := range []string{HeaderSessionKey, HeaderSessionParent, HeaderSubagent} {
		found := false
		for _, cleared := range resp.ClearHeaders {
			found = found || cleared == name
		}
		if !found {
			t.Errorf("ClearHeaders = %v, want it to include %s", resp.ClearHeaders, name)
		}
	}
}
