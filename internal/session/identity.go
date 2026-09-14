// Package session answers the two questions affinity needs: which
// conversation is this request part of, and which credential is that
// conversation already pinned to.
//
// Identity is derived from headers first and then the request body, because
// Claude Code carries its session id only in the body (metadata.user_id).
// Every key is a truncated SHA-256 so nothing client-supplied is stored or
// displayed verbatim.
package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"regexp"
	"strings"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/httpx"
)

// Identity names the conversation a request belongs to.
type Identity struct {
	// Key is the stable conversation key, empty when nothing usable was
	// found. An empty Key means no affinity is possible for this request.
	Key string
	// ParentKey is the parent conversation's key for a subagent request, and
	// is empty when the parent cannot be named.
	ParentKey string
	// Subagent marks a request issued by a child agent of another
	// conversation.
	Subagent bool
	// Source names the rule that produced Key, for the status UI.
	Source string
}

// Sources for rules that read more than one field.
const (
	SourceClaudeCodeHeader = "header:x-claude-code-session-id"
	SourceUserIDObject     = "body:metadata.user_id.session_id"
	SourceUserIDSuffix     = "body:metadata.user_id._session_"
	SourceCacheBreakpoints = "content:cache_control"
	SourceContentFallback  = "content:first-message"
)

// Claude Code's own headers. Present only when the client or a proxy in front
// of it sets them; Claude Code proper carries the session id in the body.
const (
	headerSessionID     = "X-Claude-Code-Session-Id"
	headerAgentID       = "X-Claude-Code-Agent-Id"
	headerParentAgentID = "X-Claude-Code-Parent-Agent-Id"
)

// sessionHeaders are the generic conversation headers other clients use, in
// precedence order. Every one of them names a conversation rather than a
// single request, so the key it yields survives the turn.
var sessionHeaders = []string{
	"Session-Id",
	"X-Session-Id",
	"X-Conversation-Id",
	"X-Thread-Id",
}

// mainAgentID is the agent id of a conversation's root agent. It is not a
// subagent marker.
const mainAgentID = "main"

// userIDSessionRe matches the session id in Claude Code's plain-string
// metadata.user_id, which ends in "_session_<uuid>". Clients spell the uuid in
// either case.
var userIDSessionRe = regexp.MustCompile(`(?i)_session_([a-f0-9-]+)$`)

// ephemeralCacheType is the cache_control type a client sets on the content
// block that ends a cacheable prefix.
const ephemeralCacheType = "ephemeral"

// Extract derives the conversation identity of one request.
//
// Rules run in order and the first key wins: (a) Claude Code's
// X-Claude-Code-* headers, (b) generic session headers in sessionHeaders
// order, (c) metadata.user_id, (d) other body identifier fields in bodyIDs
// order, (e) a hash of the system prompt's cache breakpoints plus the first
// message. Header lookups are case-insensitive. A body that is empty,
// truncated or not JSON contributes no key.
func Extract(headers map[string][]string, body []byte) Identity {
	h := httpx.NewIndex(headers)

	if raw, ok := fromClaudeCodeHeaders(h); ok {
		return raw.identity()
	}
	if raw, ok := fromSessionHeaders(h); ok {
		return raw.identity()
	}

	var ids bodyIDs
	if decode(body, &ids) {
		if raw, ok := fromUserID(ids); ok {
			return raw.identity()
		}
		if raw, ok := fromBodyIDs(ids); ok {
			return raw.identity()
		}
	}
	if id, ok := fromContent(body); ok {
		return id
	}
	return Identity{}
}

// decode fills v from a request body and reports whether anything usable came
// out. A syntax error means the body is truncated or not JSON and nothing was
// decoded; a type error means one field had an unexpected shape while the rest
// decoded fine, and those fields are still worth reading.
func decode(body []byte, v any) bool {
	if len(body) == 0 {
		return false
	}
	err := json.Unmarshal(body, v)
	if err == nil {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &typeErr)
}

// rawIdentity is key material, which may be prompt text. It never leaves the
// package: identity hashes it first.
type rawIdentity struct {
	key    string
	parent string
	agent  string
	source string
}

// identity hashes the material and resolves subagent status. A request is a
// subagent's when its agent id is something other than the root agent, or when
// its parent resolves to a different conversation than itself.
func (r rawIdentity) identity() Identity {
	if r.key == "" {
		return Identity{}
	}
	id := Identity{Key: hashKey(r.key), Source: r.source}
	if r.parent != "" {
		if parent := hashKey(r.parent); parent != id.Key {
			id.ParentKey = parent
			id.Subagent = true
		}
	}
	if isSubagent(r.agent) {
		id.Subagent = true
	}
	return id
}

// isSubagent reports whether an agent id names a child agent.
func isSubagent(agentID string) bool {
	return agentID != "" && !strings.EqualFold(agentID, mainAgentID)
}

// scope namespaces a conversation id by the agent running inside it, so a
// subagent gets a key of its own while the root agent keeps the bare id. It is
// the only way to separate agents that share one session id.
func scope(id, agentID string) string {
	if id == "" || !isSubagent(agentID) {
		return id
	}
	return id + "#" + agentID
}

// fromClaudeCodeHeaders reads rule a. The agent headers carry agent ids within
// one session, so both the request's key and its parent's are that session id
// scoped by the respective agent.
func fromClaudeCodeHeaders(h httpx.Index) (rawIdentity, bool) {
	sessionID := h.Get(headerSessionID)
	if sessionID == "" {
		return rawIdentity{}, false
	}
	agentID := h.Get(headerAgentID)
	parentAgentID := h.Get(headerParentAgentID)

	raw := rawIdentity{
		key:    scope(sessionID, agentID),
		agent:  agentID,
		source: SourceClaudeCodeHeader,
	}
	switch {
	case parentAgentID != "":
		raw.parent = scope(sessionID, parentAgentID)
	case isSubagent(agentID):
		// The parent is unnamed, so it is the session's root agent.
		raw.parent = sessionID
	}
	return raw, true
}

// fromSessionHeaders reads rule b.
func fromSessionHeaders(h httpx.Index) (rawIdentity, bool) {
	for _, name := range sessionHeaders {
		if v := h.Get(name); v != "" {
			return rawIdentity{key: v, source: "header:" + strings.ToLower(name)}, true
		}
	}
	return rawIdentity{}, false
}

// bodyIDs holds a request body's identifier fields. The content arrays are
// deliberately absent: encoding/json skips fields the struct does not name, so
// this decode walks a multi-megabyte body without materializing its messages.
type bodyIDs struct {
	SessionID      string `json:"session_id"`
	SessionIDCamel string `json:"sessionId"`
	PromptCacheKey string `json:"prompt_cache_key"`
	ConversationID string `json:"conversation_id"`
	ThreadID       string `json:"thread_id"`
	Metadata       struct {
		// UserID stays raw because fromUserID accepts several shapes for it.
		UserID    json.RawMessage `json:"user_id"`
		SessionID string          `json:"session_id"`
	} `json:"metadata"`
}

// userIDObject is the object shape of metadata.user_id.
type userIDObject struct {
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id"`
	AgentID         string `json:"agent_id"`
}

// fromUserID reads rule c, the Claude Code path. metadata.user_id arrives in
// three shapes: a JSON object carrying session_id, parent_session_id and
// agent_id; a JSON string holding that same object; or a plain account string
// whose "_session_<uuid>" suffix is the session id.
func fromUserID(ids bodyIDs) (rawIdentity, bool) {
	raw := bytes.TrimSpace(ids.Metadata.UserID)
	if len(raw) == 0 {
		return rawIdentity{}, false
	}

	var obj userIDObject
	var text string
	switch raw[0] {
	case '{':
		if json.Unmarshal(raw, &obj) != nil {
			return rawIdentity{}, false
		}
	case '"':
		if json.Unmarshal(raw, &text) != nil {
			return rawIdentity{}, false
		}
		text = strings.TrimSpace(text)
		if strings.HasPrefix(text, "{") {
			_ = json.Unmarshal([]byte(text), &obj)
		}
	default:
		return rawIdentity{}, false
	}

	if obj.SessionID != "" {
		out := rawIdentity{
			key:    obj.SessionID,
			parent: obj.ParentSessionID,
			agent:  obj.AgentID,
			source: SourceUserIDObject,
		}
		if out.parent == "" && isSubagent(obj.AgentID) {
			// A subagent sharing its parent's session id needs a key of its
			// own, and its parent is that session's root agent.
			out.key = scope(obj.SessionID, obj.AgentID)
			out.parent = obj.SessionID
		}
		return out, true
	}
	if m := userIDSessionRe.FindStringSubmatch(text); m != nil {
		return rawIdentity{key: m[1], source: SourceUserIDSuffix}, true
	}
	return rawIdentity{}, false
}

// fromBodyIDs reads rule d.
func fromBodyIDs(ids bodyIDs) (rawIdentity, bool) {
	candidates := []struct{ field, value string }{
		{"session_id", ids.SessionID},
		{"sessionId", ids.SessionIDCamel},
		{"metadata.session_id", ids.Metadata.SessionID},
		{"prompt_cache_key", ids.PromptCacheKey},
		{"conversation_id", ids.ConversationID},
		{"thread_id", ids.ThreadID},
	}
	for _, c := range candidates {
		if v := strings.TrimSpace(c.value); v != "" {
			return rawIdentity{key: v, source: "body:" + c.field}, true
		}
	}
	return rawIdentity{}, false
}

// bodyContent holds the content arrays the hash fallback reads.
type bodyContent struct {
	System   json.RawMessage `json:"system"`
	Messages []message       `json:"messages"`
}

type message struct {
	Role string `json:"role"`
	// Content is either a plain string or an array of blocks.
	Content json.RawMessage `json:"content"`
}

// contentBlock is one entry of a content array.
type contentBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control"`
}

// ephemeral reports whether the block ends a cacheable prefix.
func (b contentBlock) ephemeral() bool {
	return b.CacheControl != nil && b.CacheControl.Type == ephemeralCacheType
}

// fromContent reads rule e, the content fallback for clients that send no
// conversation id at all.
//
// The material is the system prompt's cache breakpoints — the whole system
// prompt when it carries none — plus the first contentful message. Both are
// fixed for the life of a conversation, which is what makes the key stable.
// Later messages are excluded: a client moves its message-level breakpoint to
// the newest turn on every request, so any material drawn from them yields a
// fresh key each turn and no affinity ever forms.
//
// A key derived from a system prompt with no message content is refused: a
// shared system prompt is not evidence of a shared conversation, and every
// Claude Code request in a workspace would collapse onto one key.
//
// The material is streamed through SHA-256 rather than concatenated, so this
// rule builds the finished key itself instead of handing material to hashKey.
func fromContent(body []byte) (Identity, bool) {
	var c bodyContent
	if !decode(body, &c) {
		return Identity{}, false
	}
	role, blocks, ok := firstContentful(c.Messages)
	if !ok {
		return Identity{}, false
	}

	system := blocksOf(c.System)
	source := SourceCacheBreakpoints
	if marked := markedBlocks(system); len(marked) > 0 {
		system = marked
	} else {
		source = SourceContentFallback
	}

	sum := sha256.New()
	writeSection(sum, "system", system)
	writeSection(sum, role, blocks)
	return Identity{Key: digest(sum), Source: source}, true
}

// markedBlocks keeps only the blocks a client marked as a cache breakpoint.
func markedBlocks(blocks []contentBlock) []contentBlock {
	var out []contentBlock
	for _, b := range blocks {
		if b.ephemeral() {
			out = append(out, b)
		}
	}
	return out
}

// firstContentful returns the first user message that carries text, falling
// back to the first message of any role that does.
func firstContentful(msgs []message) (string, []contentBlock, bool) {
	var anyRole string
	var anyBlocks []contentBlock
	for _, m := range msgs {
		blocks := blocksOf(m.Content)
		if !hasText(blocks) {
			continue
		}
		if m.Role == "user" {
			return m.Role, blocks, true
		}
		if anyBlocks == nil {
			anyRole, anyBlocks = m.Role, blocks
		}
	}
	if anyBlocks != nil {
		return anyRole, anyBlocks, true
	}
	return "", nil, false
}

func hasText(blocks []contentBlock) bool {
	for _, b := range blocks {
		if b.Text != "" {
			return true
		}
	}
	return false
}

// blocksOf reads a content field, which is either a plain string or an array
// of blocks. A block whose fields have the wrong types is skipped and the rest
// of the array still counts, matching how decode treats a mistyped field.
// Anything that is neither a string nor an array yields no blocks.
func blocksOf(raw json.RawMessage) []contentBlock {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return nil
		}
		return []contentBlock{{Type: "text", Text: s}}
	case '[':
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil {
			return nil
		}
		var blocks []contentBlock
		for _, e := range entries {
			var b contentBlock
			if json.Unmarshal(e, &b) != nil {
				continue
			}
			blocks = append(blocks, b)
		}
		return blocks
	}
	return nil
}

// Field and record separators keep concatenated material unambiguous, so two
// different block splits cannot hash alike.
const (
	fieldSep  = "\x00"
	recordSep = "\x1e"
)

// writeSection feeds one content array into the hash. Text is written block by
// block rather than joined so a large prompt is never copied whole.
func writeSection(h hash.Hash, role string, blocks []contentBlock) {
	for _, b := range blocks {
		if b.Type == "" && b.Text == "" {
			continue
		}
		writeString(h, role)
		writeString(h, fieldSep)
		writeString(h, b.Type)
		writeString(h, fieldSep)
		writeString(h, b.Text)
		writeString(h, recordSep)
	}
}

// writeString appends to a hash, which never fails.
func writeString(h hash.Hash, s string) {
	_, _ = h.Write([]byte(s))
}

// digest is the hash's own truncated hex sum, in the same shape and width
// hashKey produces, so every key the package emits is one format.
func digest(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil)[:keyBytes])
}
