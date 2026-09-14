package session

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var hexKey = regexp.MustCompile(`^[0-9a-f]{32}$`)

func hdr(kv ...string) map[string][]string {
	h := make(map[string][]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		h[kv[i]] = append(h[kv[i]], kv[i+1])
	}
	return h
}

// Test fixtures mirror the Anthropic Messages API request shape.
type tBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type tMsg struct {
	Role    string   `json:"role"`
	Content []tBlock `json:"content"`
}

type tBody struct {
	Model    string         `json:"model,omitempty"`
	System   []tBlock       `json:"system,omitempty"`
	Messages []tMsg         `json:"messages,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Stream   bool           `json:"stream,omitempty"`
}

func marked(text string) tBlock {
	return tBlock{Type: "text", Text: text, CacheControl: map[string]any{"type": "ephemeral"}}
}

func plain(text string) tBlock {
	return tBlock{Type: "text", Text: text}
}

func user(blocks ...tBlock) tMsg      { return tMsg{Role: "user", Content: blocks} }
func assistant(blocks ...tBlock) tMsg { return tMsg{Role: "assistant", Content: blocks} }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// claudeCodeBody is the shape Claude Code actually sends: the session id lives
// in metadata.user_id and in no header.
func claudeCodeBody(t *testing.T, userID any) []byte {
	t.Helper()
	return mustJSON(t, tBody{
		Model:    "claude-opus-4-6-20260514",
		System:   []tBlock{marked("You are Claude Code, Anthropic's official CLI for Claude.")},
		Messages: []tMsg{user(plain("list the files here"))},
		Metadata: map[string]any{"user_id": userID},
		Stream:   true,
	})
}

const ccUserID = "user_9f2c4a1b8e7d6c5b4a3928170f6e5d4c_account_11111111-2222-3333-4444-555555555555_session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

const ccSessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func TestExtractResolutionOrder(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string][]string
		body     []byte
		material string // key material, hashed for comparison
		source   string
	}{
		{
			name:     "claude code session header",
			headers:  hdr(headerSessionID, "sess-a"),
			material: "sess-a",
			source:   SourceClaudeCodeHeader,
		},
		{
			name:     "header lookup is case insensitive",
			headers:  hdr("x-claude-code-session-id", "sess-a"),
			material: "sess-a",
			source:   SourceClaudeCodeHeader,
		},
		{
			name: "claude code header beats every later rule",
			headers: hdr(
				headerSessionID, "sess-a",
				"Session-Id", "sess-b",
				"X-Conversation-Id", "sess-c",
			),
			body:     claudeCodeBody(t, ccUserID),
			material: "sess-a",
			source:   SourceClaudeCodeHeader,
		},
		{
			name:     "generic Session-Id",
			headers:  hdr("Session-Id", "sess-b"),
			material: "sess-b",
			source:   "header:session-id",
		},
		{
			name:     "generic X-Session-Id",
			headers:  hdr("X-Session-Id", "sess-b"),
			material: "sess-b",
			source:   "header:x-session-id",
		},
		{
			name:     "generic X-Conversation-Id",
			headers:  hdr("X-Conversation-Id", "sess-b"),
			material: "sess-b",
			source:   "header:x-conversation-id",
		},
		{
			name:     "generic X-Thread-Id",
			headers:  hdr("X-Thread-Id", "sess-b"),
			material: "sess-b",
			source:   "header:x-thread-id",
		},
		{
			name: "generic headers resolve in declared order",
			headers: hdr(
				"X-Thread-Id", "sess-d",
				"X-Conversation-Id", "sess-c",
				"X-Session-Id", "sess-b",
				"Session-Id", "sess-a",
			),
			material: "sess-a",
			source:   "header:session-id",
		},
		{
			// A request id changes every turn and would rotate the key with
			// it, and it arrives on Claude Code requests that carry a real
			// session id in the body.
			name:     "a per-request id is not a session header",
			headers:  hdr("X-Client-Request-Id", "req-1"),
			body:     claudeCodeBody(t, ccUserID),
			material: ccSessionID,
			source:   SourceUserIDSuffix,
		},
		{
			name:     "generic header beats the body",
			headers:  hdr("X-Session-Id", "sess-b"),
			body:     claudeCodeBody(t, ccUserID),
			material: "sess-b",
			source:   "header:x-session-id",
		},
		{
			name:     "blank header falls through to the body",
			headers:  hdr(headerSessionID, "   ", "X-Session-Id", ""),
			body:     claudeCodeBody(t, ccUserID),
			material: ccSessionID,
			source:   SourceUserIDSuffix,
		},
		{
			name:     "metadata.user_id suffix",
			body:     claudeCodeBody(t, ccUserID),
			material: ccSessionID,
			source:   SourceUserIDSuffix,
		},
		{
			// Clients spell the uuid in either case.
			name:     "metadata.user_id suffix in upper case",
			body:     claudeCodeBody(t, strings.ToUpper(ccUserID)),
			material: strings.ToUpper(ccSessionID),
			source:   SourceUserIDSuffix,
		},
		{
			name:     "metadata.user_id suffix with trailing whitespace",
			body:     claudeCodeBody(t, ccUserID+"  \n"),
			material: ccSessionID,
			source:   SourceUserIDSuffix,
		},
		{
			name:     "metadata.user_id object",
			body:     claudeCodeBody(t, map[string]any{"session_id": "sess-obj"}),
			material: "sess-obj",
			source:   SourceUserIDObject,
		},
		{
			name:     "metadata.user_id object carried as a string",
			body:     claudeCodeBody(t, `{"session_id":"sess-str","agent_id":"main"}`),
			material: "sess-str",
			source:   SourceUserIDObject,
		},
		{
			name: "metadata.user_id beats the other body ids",
			body: mustJSON(t, map[string]any{
				"session_id":       "sess-d",
				"prompt_cache_key": "sess-e",
				"metadata":         map[string]any{"user_id": ccUserID},
			}),
			material: ccSessionID,
			source:   SourceUserIDSuffix,
		},
		{
			name:     "body session_id",
			body:     mustJSON(t, map[string]any{"session_id": "sess-d"}),
			material: "sess-d",
			source:   "body:session_id",
		},
		{
			name:     "body sessionId",
			body:     mustJSON(t, map[string]any{"sessionId": "sess-d"}),
			material: "sess-d",
			source:   "body:sessionId",
		},
		{
			name:     "body metadata.session_id",
			body:     mustJSON(t, map[string]any{"metadata": map[string]any{"session_id": "sess-d"}}),
			material: "sess-d",
			source:   "body:metadata.session_id",
		},
		{
			name:     "body prompt_cache_key",
			body:     mustJSON(t, map[string]any{"prompt_cache_key": "sess-d"}),
			material: "sess-d",
			source:   "body:prompt_cache_key",
		},
		{
			name:     "body conversation_id",
			body:     mustJSON(t, map[string]any{"conversation_id": "sess-d"}),
			material: "sess-d",
			source:   "body:conversation_id",
		},
		{
			name:     "body thread_id",
			body:     mustJSON(t, map[string]any{"thread_id": "sess-d"}),
			material: "sess-d",
			source:   "body:thread_id",
		},
		{
			name: "body ids resolve in declared order",
			body: mustJSON(t, map[string]any{
				"thread_id":        "sess-f",
				"conversation_id":  "sess-e",
				"prompt_cache_key": "sess-d",
				"sessionId":        "sess-c",
				"session_id":       "sess-b",
				"metadata":         map[string]any{"session_id": "sess-a"},
			}),
			material: "sess-b",
			source:   "body:session_id",
		},
		{
			name: "body id beats the content hash",
			body: mustJSON(t, tBody{
				Model:    "claude-opus-4-6-20260514",
				System:   []tBlock{marked("system")},
				Messages: []tMsg{user(marked("hello"))},
				Metadata: map[string]any{"session_id": "sess-d"},
			}),
			material: "sess-d",
			source:   "body:metadata.session_id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(tc.headers, tc.body)
			if want := wantKey(tc.source, tc.material); got.Key != want {
				t.Errorf("Key = %q, want %q (hash of %q under %q)", got.Key, want, tc.material, tc.source)
			}
			if got.Source != tc.source {
				t.Errorf("Source = %q, want %q", got.Source, tc.source)
			}
			if got.Subagent {
				t.Errorf("Subagent = true, want false")
			}
			if got.ParentKey != "" {
				t.Errorf("ParentKey = %q, want empty", got.ParentKey)
			}
		})
	}
}

func TestExtractSubagent(t *testing.T) {
	tests := []struct {
		name           string
		headers        map[string][]string
		body           []byte
		keyMaterial    string
		parentMaterial string // empty means ParentKey must be empty
		subagent       bool
	}{
		{
			name:        "agent id main is not a subagent",
			headers:     hdr(headerSessionID, "sess-a", headerAgentID, "main"),
			keyMaterial: "sess-a",
		},
		{
			name:        "agent id main is matched case insensitively",
			headers:     hdr(headerSessionID, "sess-a", headerAgentID, "MAIN"),
			keyMaterial: "sess-a",
		},
		{
			name:        "absent agent id is not a subagent",
			headers:     hdr(headerSessionID, "sess-a"),
			keyMaterial: "sess-a",
		},
		{
			name:           "agent id alone marks a subagent and links the session root",
			headers:        hdr(headerSessionID, "sess-a", headerAgentID, "explore"),
			keyMaterial:    "6:sess-a#explore",
			parentMaterial: "sess-a",
			subagent:       true,
		},
		{
			name: "parent agent id names the parent inside the session",
			headers: hdr(
				headerSessionID, "sess-a",
				headerAgentID, "explore",
				headerParentAgentID, "plan",
			),
			keyMaterial:    "6:sess-a#explore",
			parentMaterial: "6:sess-a#plan",
			subagent:       true,
		},
		{
			name: "parent agent id main links the session root",
			headers: hdr(
				headerSessionID, "sess-a",
				headerAgentID, "explore",
				headerParentAgentID, "main",
			),
			keyMaterial:    "6:sess-a#explore",
			parentMaterial: "sess-a",
			subagent:       true,
		},
		{
			name:           "parent session id alone marks a subagent",
			body:           claudeCodeBody(t, map[string]any{"session_id": "child", "parent_session_id": "parent"}),
			keyMaterial:    "child",
			parentMaterial: "parent",
			subagent:       true,
		},
		{
			name: "agent id and parent session id together",
			body: claudeCodeBody(t, map[string]any{
				"session_id":        "child",
				"parent_session_id": "parent",
				"agent_id":          "explore",
			}),
			keyMaterial:    "child",
			parentMaterial: "parent",
			subagent:       true,
		},
		{
			name:           "agent id without a parent session id scopes the session",
			body:           claudeCodeBody(t, map[string]any{"session_id": "sess-a", "agent_id": "explore"}),
			keyMaterial:    "6:sess-a#explore",
			parentMaterial: "sess-a",
			subagent:       true,
		},
		{
			name:        "agent id main in the body is not a subagent",
			body:        claudeCodeBody(t, map[string]any{"session_id": "sess-a", "agent_id": "main"}),
			keyMaterial: "sess-a",
		},
		{
			name:        "parent session id equal to the session id is not a subagent",
			body:        claudeCodeBody(t, map[string]any{"session_id": "sess-a", "parent_session_id": "sess-a"}),
			keyMaterial: "sess-a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(tc.headers, tc.body)
			source := SourceClaudeCodeHeader
			if tc.headers == nil {
				source = SourceUserIDObject
			}
			if got.Source != source {
				t.Fatalf("Source = %q, want %q", got.Source, source)
			}
			if want := wantKey(source, tc.keyMaterial); got.Key != want {
				t.Errorf("Key = %q, want %q (hash of %q under %q)", got.Key, want, tc.keyMaterial, source)
			}
			if got.Subagent != tc.subagent {
				t.Errorf("Subagent = %v, want %v", got.Subagent, tc.subagent)
			}
			wantParent := ""
			if tc.parentMaterial != "" {
				wantParent = wantKey(source, tc.parentMaterial)
			}
			if got.ParentKey != wantParent {
				t.Errorf("ParentKey = %q, want %q", got.ParentKey, wantParent)
			}
		})
	}
}

// Clients move the message-level cache breakpoint to the newest turn on every
// request, which is Anthropic's documented multi-turn pattern. The key covers
// the system prompt's breakpoints and the first message only, so it survives
// the marker moving.
func TestExtractContentKeyIsStableAcrossTurns(t *testing.T) {
	sys := marked("You are Claude Code, Anthropic's official CLI for Claude.")
	const opening = "read internal/session/identity.go"

	turns := [][]byte{
		mustJSON(t, tBody{System: []tBlock{sys}, Messages: []tMsg{
			user(marked(opening)),
		}}),
		mustJSON(t, tBody{System: []tBlock{sys}, Messages: []tMsg{
			user(plain(opening)),
			assistant(plain("Here is the file.")),
			user(marked("now the tests")),
		}}),
		mustJSON(t, tBody{System: []tBlock{sys}, Messages: []tMsg{
			user(plain(opening)),
			assistant(plain("Here is the file.")),
			user(plain("now the tests")),
			assistant(plain("Here are the tests.")),
			user(marked("and the store")),
		}}),
	}

	got := Extract(nil, turns[0])
	if got.Source != SourceCacheBreakpoints {
		t.Fatalf("Source = %q, want %q", got.Source, SourceCacheBreakpoints)
	}
	if !hexKey.MatchString(got.Key) {
		t.Fatalf("Key = %q, want 32 hex chars", got.Key)
	}
	for i, body := range turns[1:] {
		if k := Extract(nil, body).Key; k != got.Key {
			t.Errorf("turn %d moved the marker and changed the key: %q != %q", i+2, k, got.Key)
		}
	}

	otherOpening := mustJSON(t, tBody{System: []tBlock{sys}, Messages: []tMsg{
		user(marked("read internal/session/store.go")),
	}})
	if k := Extract(nil, otherOpening).Key; k == got.Key {
		t.Errorf("a different first message kept key %q", k)
	}

	otherSystem := mustJSON(t, tBody{
		System:   []tBlock{marked("You are a different assistant.")},
		Messages: []tMsg{user(marked(opening))},
	})
	if k := Extract(nil, otherSystem).Key; k == got.Key {
		t.Errorf("a different marked system prompt kept key %q", k)
	}

	// Only the marked system blocks count, so text after the breakpoint —
	// which a client rewrites per request — is outside the key.
	trailingSystem := mustJSON(t, tBody{
		System:   []tBlock{sys, plain("Today is 2026-09-04.")},
		Messages: []tMsg{user(marked(opening))},
	})
	if k := Extract(nil, trailingSystem).Key; k != got.Key {
		t.Errorf("an unmarked trailing system block changed the key: %q != %q", k, got.Key)
	}
}

// Without a marked system block the hash falls back to the whole system prompt
// plus the first message.
func TestExtractContentFallback(t *testing.T) {
	sys := plain("shared workspace system prompt")

	first := mustJSON(t, tBody{System: []tBlock{sys}, Messages: []tMsg{
		user(plain("first question")),
		assistant(plain("first answer")),
	}})
	sameFirst := mustJSON(t, tBody{System: []tBlock{sys}, Messages: []tMsg{
		user(plain("first question")),
		assistant(plain("a different answer")),
		user(plain("second question")),
	}})
	otherFirst := mustJSON(t, tBody{System: []tBlock{sys}, Messages: []tMsg{
		user(plain("a different first question")),
	}})

	got := Extract(nil, first)
	if got.Source != SourceContentFallback {
		t.Fatalf("Source = %q, want %q", got.Source, SourceContentFallback)
	}
	if !hexKey.MatchString(got.Key) {
		t.Fatalf("Key = %q, want 32 hex chars", got.Key)
	}
	if k := Extract(nil, sameFirst).Key; k != got.Key {
		t.Errorf("same system and first message gave %q, want %q", k, got.Key)
	}
	if k := Extract(nil, otherFirst).Key; k == got.Key {
		t.Errorf("a different first message kept key %q", k)
	}
}

// String content is equivalent to a single text block, which is how a plain
// system prompt and a plain message body arrive.
func TestExtractContentFallbackStringContent(t *testing.T) {
	body := []byte(`{"system":"you are helpful","messages":[{"role":"user","content":"hi there"}]}`)
	got := Extract(nil, body)
	if got.Source != SourceContentFallback {
		t.Fatalf("Source = %q, want %q", got.Source, SourceContentFallback)
	}
	if !hexKey.MatchString(got.Key) {
		t.Errorf("Key = %q, want 32 hex chars", got.Key)
	}
}

// A shared system prompt is not evidence of a shared conversation, so a key
// derived from one with no message content is refused.
func TestExtractSystemOnlyYieldsNoKey(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "marked system prompt and no messages",
			body: mustJSON(t, tBody{System: []tBlock{marked("You are Claude Code.")}}),
		},
		{
			name: "unmarked system prompt and no messages",
			body: mustJSON(t, tBody{System: []tBlock{plain("You are Claude Code.")}}),
		},
		{
			name: "system prompt with an empty message list",
			body: []byte(`{"system":"You are Claude Code.","messages":[]}`),
		},
		{
			name: "system prompt with a message carrying no text",
			body: []byte(`{"system":"You are Claude Code.","messages":[{"role":"user","content":[]}]}`),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Extract(nil, tc.body); got != (Identity{}) {
				t.Errorf("Extract = %+v, want zero Identity", got)
			}
		})
	}
}

func TestExtractUnusableInput(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string][]string
		body    []byte
	}{
		{name: "nil body", body: nil},
		{name: "empty body", body: []byte{}},
		{name: "empty header map", headers: map[string][]string{}, body: []byte(`{}`)},
		{name: "whitespace", body: []byte("   \n\t ")},
		{name: "not json", body: []byte("this is not json at all")},
		{name: "html error page", body: []byte("<html><body>502</body></html>")},
		{name: "truncated object", body: []byte(`{"metadata":{"user_id":"user_x_session_abc`)},
		{name: "truncated messages", body: []byte(`{"system":"s","messages":[{"role":"user","content":[{"type":"tex`)},
		{name: "bare string", body: []byte(`"just a string"`)},
		{name: "bare number", body: []byte(`12345`)},
		{name: "top level array", body: []byte(`[1,2,3]`)},
		{name: "null", body: []byte(`null`)},
		{name: "empty object", body: []byte(`{}`)},
		{name: "user_id is a number", body: []byte(`{"metadata":{"user_id":123}}`)},
		{name: "user_id is an array", body: []byte(`{"metadata":{"user_id":[1,2]}}`)},
		{name: "user_id is an empty object", body: []byte(`{"metadata":{"user_id":{}}}`)},
		{name: "user_id has no session suffix", body: []byte(`{"metadata":{"user_id":"user_abc_account_def"}}`)},
		{name: "user_id suffix is not hex", body: []byte(`{"metadata":{"user_id":"user_abc_session_ZZZ!"}}`)},
		{name: "user_id is a broken json string", body: []byte(`{"metadata":{"user_id":"{\"session_id\":"}}`)},
		{name: "metadata is a string", body: []byte(`{"metadata":"nope"}`)},
		{name: "ids are blank", body: []byte(`{"session_id":"","conversation_id":"   "}`)},
		{name: "messages is not an array", body: []byte(`{"messages":5}`)},
		{name: "content is a number", body: []byte(`{"messages":[{"role":"user","content":7}]}`)},
		{name: "deeply nested noise", body: []byte(`{"a":{"b":{"c":{"d":[{"e":"f"}]}}}}`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Extract(tc.headers, tc.body); got != (Identity{}) {
				t.Errorf("Extract = %+v, want zero Identity", got)
			}
		})
	}
}

// A block with a mistyped field is skipped and the rest of the array still
// counts, matching how a mistyped top-level field is treated.
func TestExtractContentSkipsMistypedBlocks(t *testing.T) {
	good := []byte(`{"system":"s","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"hello"}]}]}`)
	withBadBlock := []byte(`{"system":"s","messages":[{"role":"user","content":[` +
		`{"type":"text","text":5},{"type":"text","text":"hello"}]}]}`)

	want := Extract(nil, good)
	if want.Source != SourceContentFallback || !hexKey.MatchString(want.Key) {
		t.Fatalf("Extract = %+v, want a %q key", want, SourceContentFallback)
	}
	if got := Extract(nil, withBadBlock); got != want {
		t.Errorf("Extract = %+v, want %+v; a mistyped block discarded the array", got, want)
	}
}

// Keys are opaque and bounded so nothing prompt-derived reaches the binding
// table or the status UI.
func TestExtractKeysAreOpaqueAndBounded(t *testing.T) {
	secret := "SECRET-PROMPT-TEXT-do-not-leak"
	bodies := [][]byte{
		claudeCodeBody(t, ccUserID),
		claudeCodeBody(t, map[string]any{"session_id": secret, "agent_id": secret + "-agent"}),
		mustJSON(t, map[string]any{"session_id": secret}),
		mustJSON(t, map[string]any{"prompt_cache_key": secret}),
		mustJSON(t, tBody{System: []tBlock{marked(secret)}, Messages: []tMsg{user(marked(secret))}}),
		mustJSON(t, tBody{System: []tBlock{plain(secret)}, Messages: []tMsg{user(plain(secret))}}),
	}
	headerSets := []map[string][]string{
		nil,
		hdr(headerSessionID, secret, headerAgentID, secret+"-agent"),
		hdr("X-Conversation-Id", secret),
	}

	for _, h := range headerSets {
		for i, body := range bodies {
			got := Extract(h, body)
			if got.Key == "" {
				t.Fatalf("headers=%v body#%d: empty key", h, i)
			}
			for name, key := range map[string]string{"Key": got.Key, "ParentKey": got.ParentKey} {
				if key == "" {
					continue
				}
				if !hexKey.MatchString(key) {
					t.Errorf("headers=%v body#%d: %s = %q, want 32 hex chars", h, i, name, key)
				}
				if strings.Contains(key, secret) || strings.Contains(key, ccSessionID) {
					t.Errorf("headers=%v body#%d: %s = %q leaks input", h, i, name, key)
				}
			}
		}
	}
}

// Multi-megabyte bodies are routine, and the identifier rules must resolve
// without choking on them.
func TestExtractLargeBody(t *testing.T) {
	filler := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100_000) // ~4.5 MB

	withID := mustJSON(t, tBody{
		System:   []tBlock{marked(filler)},
		Messages: []tMsg{user(marked(filler)), assistant(plain(filler))},
		Metadata: map[string]any{"user_id": ccUserID},
	})
	if len(withID) < 4<<20 {
		t.Fatalf("fixture is only %d bytes", len(withID))
	}
	if got := Extract(nil, withID); got.Key != wantKey(SourceUserIDSuffix, ccSessionID) || got.Source != SourceUserIDSuffix {
		t.Errorf("Extract = %+v, want session %q from %q", got, ccSessionID, SourceUserIDSuffix)
	}

	noID := mustJSON(t, tBody{
		System:   []tBlock{marked(filler)},
		Messages: []tMsg{user(marked(filler)), assistant(plain(filler))},
	})
	got := Extract(nil, noID)
	if got.Source != SourceCacheBreakpoints || !hexKey.MatchString(got.Key) {
		t.Errorf("Extract = %+v, want a %q key", got, SourceCacheBreakpoints)
	}
}

// benchBody is a multi-megabyte request in the shape both benchmarks measure:
// a marked system prompt and one long marked user turn. metadata carries the
// user id only when withUserID, which is what separates the identifier path
// from the content path.
func benchBody(withUserID bool) []byte {
	metadata := ""
	if withUserID {
		metadata = `"metadata":{"user_id":"` + ccUserID + `"},`
	}
	return []byte(`{"model":"claude-opus-4-6-20260514",` + metadata +
		`"system":[{"type":"text","text":"` + strings.Repeat("system prompt ", 20_000) + `","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("turn ", 100_000) + `","cache_control":{"type":"ephemeral"}}]}]}`)
}

func BenchmarkExtractClaudeCode(b *testing.B) {
	benchmarkExtract(b, benchBody(true))
}

func BenchmarkExtractContentHash(b *testing.B) {
	benchmarkExtract(b, benchBody(false))
}

func benchmarkExtract(b *testing.B, body []byte) {
	b.Helper()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if Extract(nil, body).Key == "" {
			b.Fatal("no key")
		}
	}
}

// TestUserIDObjectIDsAreTrimmed covers a whitespace-only id in
// metadata.user_id: untrimmed it forms a normal-looking key that every client
// emitting one shares, pinning all of that traffic to one seat, and a
// whitespace agent id marks every such request a subagent's.
func TestUserIDObjectIDsAreTrimmed(t *testing.T) {
	blank := claudeCodeBody(t, map[string]any{"session_id": " \t\n ", "agent_id": "  "})
	got := Extract(nil, blank)
	if got.Source == SourceUserIDObject {
		t.Errorf("Extract = %+v, want a whitespace-only session id to read as absent", got)
	}
	if got.Subagent {
		t.Errorf("Extract = %+v, want a whitespace-only agent id to name no subagent", got)
	}

	// A padded id resolves to the same conversation as the bare one.
	padded := claudeCodeBody(t, map[string]any{"session_id": "  sess-obj \n"})
	bare := claudeCodeBody(t, map[string]any{"session_id": "sess-obj"})
	if a, b := Extract(nil, padded), Extract(nil, bare); a.Key != b.Key || a.Key != wantKey(SourceUserIDObject, "sess-obj") {
		t.Errorf("padded = %+v, bare = %+v, want both keyed on the trimmed id", a, b)
	}

	// Padding around an agent id does not split a subagent off its parent.
	sub := claudeCodeBody(t, map[string]any{"session_id": "sess-obj", "agent_id": " worker "})
	flat := claudeCodeBody(t, map[string]any{"session_id": "sess-obj", "agent_id": "worker"})
	if a, b := Extract(nil, sub), Extract(nil, flat); a.Key != b.Key || !a.Subagent {
		t.Errorf("padded agent = %+v, bare agent = %+v, want one subagent key", a, b)
	}
}

// wantKey is the key a rule produces for its material. The rule that found the
// material namespaces the hash, so the same string under two identifiers names
// two conversations.
func wantKey(source, material string) string {
	return hashKey(source + fieldSep + material)
}

// TestKeysAreNamespacedByRule covers one string arriving under three
// identifiers: unnamespaced they hash alike and three unrelated conversations
// share a seat binding.
func TestKeysAreNamespacedByRule(t *testing.T) {
	keys := map[string]string{}
	for _, c := range []struct {
		name    string
		headers map[string][]string
		body    []byte
	}{
		{"thread header", hdr("X-Thread-Id", "1"), nil},
		{"session header", hdr("X-Session-Id", "1"), nil},
		{"conversation_id", nil, mustJSON(t, map[string]any{"conversation_id": "1"})},
		{"prompt_cache_key", nil, mustJSON(t, map[string]any{"prompt_cache_key": "1"})},
	} {
		got := Extract(c.headers, c.body)
		if got.Key == "" {
			t.Fatalf("%s: no key", c.name)
		}
		if prior, seen := keys[got.Key]; seen {
			t.Errorf("%s collides with %s on key %q", c.name, prior, got.Key)
		}
		keys[got.Key] = c.name
	}
}

// TestScopedKeysDoNotCollideWithBareOnes covers the agent separator appearing
// in a session id of its own: without the length prefix session "a" running
// agent "b" and the bare session "a#b" name one conversation.
func TestScopedKeysDoNotCollideWithBareOnes(t *testing.T) {
	scoped := Extract(hdr(headerSessionID, "a", headerAgentID, "b"), nil)
	bare := Extract(hdr(headerSessionID, "a#b"), nil)
	if scoped.Key == bare.Key {
		t.Errorf("session %q under agent %q and bare session %q share key %q", "a", "b", "a#b", scoped.Key)
	}
	if scope("a", "b#c") == scope("a#b", "c") {
		t.Errorf("scope is ambiguous: session %q under agent %q reads as %q under %q", "a", "b#c", "a#b", "c")
	}
}

// TestHeaderIndexIsDeterministicAcrossCasings covers a request carrying two
// capitalizations of one header: a map range visits them in a different order
// every time, so an index without a tie-break hands the conversation a
// different key per request and affinity loses it.
func TestHeaderIndexIsDeterministicAcrossCasings(t *testing.T) {
	headers := map[string][]string{
		"Session-Id": {"upper"},
		"session-id": {"lower"},
		"SESSION-ID": {"shout"},
	}
	first := Extract(headers, nil)
	if first.Key == "" {
		t.Fatal("no key")
	}
	for i := 0; i < 200; i++ {
		if got := Extract(headers, nil); got.Key != first.Key {
			t.Fatalf("call %d gave key %q, first call gave %q", i, got.Key, first.Key)
		}
	}
	if want := wantKey("header:session-id", "shout"); first.Key != want {
		t.Errorf("key = %q, want the value under the first-sorting spelling", first.Key)
	}
}
