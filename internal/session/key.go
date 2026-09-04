package session

import (
	"crypto/sha256"
	"encoding/hex"
)

// keyBytes is how much of a SHA-256 a conversation key carries: 16 bytes, so
// 32 hex characters. Wide enough that collisions are irrelevant at any
// realistic session count, narrow enough to print in the status UI.
const keyBytes = 16

// hashKey turns arbitrary material into a bounded, opaque key.
//
// Keys are hashed because the material may be prompt text: the content
// fallback derives a key from the request's content blocks, and a key is both
// stored in the binding table and displayed in the status UI. Hashing is what
// keeps prompt text out of both.
func hashKey(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:keyBytes])
}

// BindingKey composes the binding-table key for one conversation on one
// provider and model.
//
// A binding is per provider and per model as well as per conversation, because
// a model can be served by a different credential set than its siblings: a
// provider-blind or model-blind key hands back a credential that cannot serve
// the request. The session key is already opaque and bounded, and provider and
// model are host-supplied identifiers, so the composite needs no hashing.
func BindingKey(provider, modelID, sessionKey string) string {
	return provider + "|" + modelID + "|" + sessionKey
}
