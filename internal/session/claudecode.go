package session

import (
	"slices"
	"strings"
)

// ClaudeCodeKeys returns every key a Claude Code session's root agent can be
// bound under: one per rule that reads its session id, the
// X-Claude-Code-Session-Id header and the two shapes of metadata.user_id. A
// client sends one of them, so at most one is bound. The suffix rule keeps
// the id's case, and a client may spell it in either, so both spellings count.
func ClaudeCodeKeys(sessionID string) []string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	keys := []string{
		hashKey(SourceClaudeCodeHeader + fieldSep + sessionID),
		hashKey(SourceUserIDObject + fieldSep + sessionID),
	}
	for _, id := range []string{sessionID, strings.ToLower(sessionID), strings.ToUpper(sessionID)} {
		if key := hashKey(SourceUserIDSuffix + fieldSep + id); !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}
