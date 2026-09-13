package runtime

import (
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// BindingStore is the conversation-to-credential table the scheduler pins
// sessions with. session.Store satisfies it; tests substitute an in-memory
// fake. Every method must be safe for concurrent use, because scheduler.pick
// runs with no serialization from the host.
//
// A binding is scoped per provider and model as well as per conversation:
// prompt caches are per model and a model may be served by a different
// credential set than its siblings.
type BindingStore interface {
	// Lookup returns the binding for a conversation and refreshes its idle
	// timer on a hit.
	Lookup(provider, modelID, sessionKey string, now time.Time) (model.Binding, bool)
	// Bind pins a conversation to a credential, replacing any prior binding.
	Bind(provider, modelID, sessionKey, authID string, now time.Time) model.Binding
	// Drop removes one binding.
	Drop(provider, modelID, sessionKey string)
	// DropAuth unbinds every conversation on a credential and reports how many.
	DropAuth(authID string) int
	// All returns every binding, newest LastSeen first.
	All() []model.Binding
	// CountByAuth reports live bindings per credential id.
	CountByAuth() map[string]int
	// Len reports how many bindings are held.
	Len() int
	// Sweep removes bindings idle past the TTL and reports how many.
	Sweep(now time.Time) int
}

// NewBindingStore constructs the binding table for an affinity configuration.
// It is called at registration and again whenever the TTL or session cap
// changes, which discards the bindings held under the old settings.
type NewBindingStore func(ttl time.Duration, maxSessions int) BindingStore
