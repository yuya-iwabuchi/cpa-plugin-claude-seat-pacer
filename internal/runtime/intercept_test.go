package runtime

import (
	"encoding/json"
	"regexp"
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

func TestInterceptBeforeReturnsNothingWithoutIdentity(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	for name, body := range map[string][]byte{
		"no ids and no content": []byte(`{"model":"claude-opus-5","messages":[]}`),
		"empty body":            nil,
		"not json":              []byte("<html>"),
	} {
		t.Run(name, func(t *testing.T) {
			raw, ok := tp.Call(MethodRequestInterceptBefore, interceptPayload(t, body))
			if !ok {
				t.Fatalf("identity miss produced an error envelope: %s", raw)
			}
			var env Envelope
			_ = json.Unmarshal(raw, &env)
			var out map[string]json.RawMessage
			_ = json.Unmarshal(env.Result, &out)
			if len(out) != 0 {
				t.Errorf("result = %s, want {}", env.Result)
			}
		})
	}
}

func TestInterceptBeforeToleratesUndecodablePayload(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	if raw, ok := tp.Call(MethodRequestInterceptBefore, []byte("garbage")); !ok {
		t.Fatalf("garbage payload produced an error envelope: %s", raw)
	}
}
