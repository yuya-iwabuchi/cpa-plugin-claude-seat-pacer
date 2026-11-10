package session

import (
	"slices"
	"strings"
	"testing"
)

func TestClaudeCodeKeysMatchEveryClaudeCodeShape(t *testing.T) {
	const id = "0b2e7c1a-4d36-4f58-9a7e-3c1f5d8e2b40"
	keys := ClaudeCodeKeys(id)
	shapes := map[string]Identity{
		"header":         Extract(map[string][]string{"X-Claude-Code-Session-Id": {id}}, nil),
		"user_id object": Extract(nil, []byte(`{"metadata":{"user_id":{"session_id":"`+id+`"}}}`)),
		"user_id string": Extract(nil, []byte(`{"metadata":{"user_id":"{\"session_id\":\"`+id+`\"}"}}`)),
		"user_id suffix": Extract(nil, []byte(`{"metadata":{"user_id":"user_abc_account_def_session_`+id+`"}}`)),
		"upper suffix":   Extract(nil, []byte(`{"metadata":{"user_id":"user_abc_account_def_session_`+strings.ToUpper(id)+`"}}`)),
	}
	for name, got := range shapes {
		if !slices.Contains(keys, got.Key) {
			t.Errorf("%s: key %q is not among %v", name, got.Key, keys)
		}
	}
}

func TestClaudeCodeKeysLeaveSubagentsOut(t *testing.T) {
	const id = "0b2e7c1a-4d36-4f58-9a7e-3c1f5d8e2b40"
	sub := Extract(nil, []byte(`{"metadata":{"user_id":{"session_id":"`+id+`","agent_id":"a1"}}}`))
	if slices.Contains(ClaudeCodeKeys(id), sub.Key) {
		t.Fatalf("a subagent's key %q matched its session's root keys", sub.Key)
	}
}

func TestClaudeCodeKeysOfNoSession(t *testing.T) {
	if keys := ClaudeCodeKeys("  "); keys != nil {
		t.Fatalf("got %v for a blank session id", keys)
	}
}
