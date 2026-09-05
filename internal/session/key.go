package session

import (
	"crypto/sha256"
	"encoding/hex"
)

// keyBytes is how much of a SHA-256 a conversation key carries: 16 bytes, so
// 32 hex characters. Wide enough that collisions are irrelevant at any
// realistic session count, narrow enough to print in the status UI.
const keyBytes = 16

// hashKey turns client-supplied material — header values, body ids, agent ids
// — into a bounded, opaque key, so an unbounded or sensitive identifier never
// reaches the binding table or the status UI.
func hashKey(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:keyBytes])
}
