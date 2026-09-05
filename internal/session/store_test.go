package session

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// Most tests exercise one provider and model, so these wrappers keep the
// conversation key the only varying part.
func bind(s *Store, sessionKey, authID string, now time.Time) model.Binding {
	return s.Bind("claude", "opus", sessionKey, authID, now)
}

func lookup(s *Store, sessionKey string, now time.Time) (model.Binding, bool) {
	return s.Lookup("claude", "opus", sessionKey, now)
}

func TestStoreBindAndLookup(t *testing.T) {
	s := NewStore(time.Hour, 8)

	b := bind(s, "k1", "auth-1", t0)
	if b.SessionKey != "k1" || b.Provider != "claude" || b.Model != "opus" || b.AuthID != "auth-1" {
		t.Fatalf("Bind = %+v", b)
	}
	if !b.BoundAt.Equal(t0) || !b.LastSeen.Equal(t0) || b.Hits != 0 {
		t.Fatalf("Bind = %+v, want BoundAt and LastSeen at t0 with no hits", b)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}

	got, ok := lookup(s, "k1", at(time.Minute))
	if !ok {
		t.Fatal("Lookup missed a fresh binding")
	}
	if got.Hits != 1 || !got.LastSeen.Equal(at(time.Minute)) || !got.BoundAt.Equal(t0) {
		t.Errorf("Lookup = %+v, want one hit, refreshed LastSeen, original BoundAt", got)
	}
	if got, ok := lookup(s, "k1", at(2*time.Minute)); !ok || got.Hits != 2 {
		t.Errorf("second Lookup = %+v ok=%v, want two hits", got, ok)
	}
	if _, ok := lookup(s, "missing", t0); ok {
		t.Error("Lookup hit an unknown key")
	}
}

// An empty session key names no conversation, so binding it would pin every
// unidentifiable request to one credential.
func TestStoreEmptySessionKey(t *testing.T) {
	s := NewStore(time.Hour, 8)

	if b := bind(s, "", "auth-1", t0); b != (model.Binding{}) {
		t.Errorf("Bind with an empty key = %+v, want a zero Binding", b)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want the empty key to bind nothing", s.Len())
	}
	if _, ok := lookup(s, "", t0); ok {
		t.Error("Lookup hit on an empty key")
	}
}

// A binding is per provider and model as well as per conversation, because a
// model can be served by a different credential set than its siblings.
func TestStoreScopedByProviderAndModel(t *testing.T) {
	s := NewStore(time.Hour, 8)
	id := Extract(nil, []byte(`{"session_id":"sess-a"}`))

	s.Bind("claude", "opus", id.Key, "auth-1", t0)
	s.Bind("claude", "sonnet", id.Key, "auth-2", t0)
	s.Bind("other", "opus", id.Key, "auth-3", t0)

	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3", s.Len())
	}
	for _, tc := range []struct{ provider, modelID, authID string }{
		{"claude", "opus", "auth-1"},
		{"claude", "sonnet", "auth-2"},
		{"other", "opus", "auth-3"},
	} {
		b, ok := s.Lookup(tc.provider, tc.modelID, id.Key, t0)
		if !ok {
			t.Fatalf("Lookup(%s, %s) missed", tc.provider, tc.modelID)
		}
		if b.AuthID != tc.authID || b.Provider != tc.provider || b.Model != tc.modelID {
			t.Errorf("Lookup(%s, %s) = %+v, want %q", tc.provider, tc.modelID, b, tc.authID)
		}
		if b.SessionKey != id.Key {
			t.Errorf("SessionKey = %q, want the bare conversation key %q", b.SessionKey, id.Key)
		}
	}

	s.Drop("claude", "opus", id.Key)
	if _, ok := s.Lookup("claude", "opus", id.Key, t0); ok {
		t.Error("Drop left the binding reachable")
	}
	if _, ok := s.Lookup("claude", "sonnet", id.Key, t0); !ok {
		t.Error("Drop removed another model's binding")
	}
}

// The TTL bounds idle time, not total session length, so a conversation that
// keeps being used never expires.
func TestStoreTTLIsIdleTime(t *testing.T) {
	s := NewStore(10*time.Minute, 8)
	bind(s, "k1", "auth-1", t0)

	for _, d := range []time.Duration{9 * time.Minute, 18 * time.Minute, 27 * time.Minute} {
		if _, ok := lookup(s, "k1", at(d)); !ok {
			t.Fatalf("Lookup at +%v missed; TTL is bounding total length, not idle time", d)
		}
	}
	if _, ok := lookup(s, "k1", at(38*time.Minute)); ok {
		t.Error("Lookup hit a binding idle past the TTL")
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want the expired binding dropped", s.Len())
	}
}

func TestStoreExpiryBoundary(t *testing.T) {
	tests := []struct {
		name string
		idle time.Duration
		want bool
	}{
		{name: "just inside the TTL", idle: 10*time.Minute - time.Nanosecond, want: true},
		{name: "exactly at the TTL", idle: 10 * time.Minute, want: true},
		{name: "just past the TTL", idle: 10*time.Minute + time.Nanosecond, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(10*time.Minute, 8)
			bind(s, "k1", "auth-1", t0)
			if _, ok := lookup(s, "k1", at(tc.idle)); ok != tc.want {
				t.Errorf("Lookup after %v = %v, want %v", tc.idle, ok, tc.want)
			}
		})
	}
}

func TestStoreRebind(t *testing.T) {
	s := NewStore(time.Hour, 8)
	bind(s, "k1", "auth-1", t0)
	lookup(s, "k1", at(time.Minute))

	// Rebinding to the same credential is a refresh, so the cached prefix
	// history survives.
	same := bind(s, "k1", "auth-1", at(2*time.Minute))
	if !same.BoundAt.Equal(t0) || same.Hits != 1 || !same.LastSeen.Equal(at(2*time.Minute)) {
		t.Errorf("re-Bind to the same credential = %+v, want the existing binding refreshed", same)
	}

	// A different credential is a different cached prefix.
	moved := bind(s, "k1", "auth-2", at(3*time.Minute))
	if moved.AuthID != "auth-2" || !moved.BoundAt.Equal(at(3*time.Minute)) || moved.Hits != 0 {
		t.Errorf("re-Bind to another credential = %+v, want a fresh binding", moved)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

// A key whose binding has gone idle past the TTL names a conversation that is
// over, so rebinding it starts a new one rather than inheriting its counters.
func TestStoreBindTreatsExpiredAsAbsent(t *testing.T) {
	s := NewStore(10*time.Minute, 8)
	bind(s, "k1", "auth-1", t0)
	lookup(s, "k1", at(time.Minute))

	revived := bind(s, "k1", "auth-1", at(time.Hour))
	if !revived.BoundAt.Equal(at(time.Hour)) || revived.Hits != 0 {
		t.Errorf("re-Bind after the TTL = %+v, want a fresh binding at +1h with no hits", revived)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestStoreLRUEviction(t *testing.T) {
	s := NewStore(time.Hour, 2)
	bind(s, "k1", "auth-1", t0)
	bind(s, "k2", "auth-2", at(time.Minute))

	// Touching k1 makes k2 the least recently seen.
	if _, ok := lookup(s, "k1", at(2*time.Minute)); !ok {
		t.Fatal("Lookup missed k1")
	}
	bind(s, "k3", "auth-3", at(3*time.Minute))

	if s.Len() != 2 {
		t.Fatalf("Len = %d, want the cap of 2", s.Len())
	}
	if _, ok := lookup(s, "k2", at(3*time.Minute)); ok {
		t.Error("k2 survived; eviction did not take the least recently seen")
	}
	for _, key := range []string{"k1", "k3"} {
		if _, ok := lookup(s, key, at(3*time.Minute)); !ok {
			t.Errorf("%s was evicted", key)
		}
	}
}

func TestStoreEvictionHoldsTheCap(t *testing.T) {
	s := NewStore(time.Hour, 4)
	for i := range 50 {
		bind(s, fmt.Sprintf("k%02d", i), "auth-1", at(time.Duration(i)*time.Second))
		if s.Len() > 4 {
			t.Fatalf("Len = %d after %d binds, want at most 4", s.Len(), i+1)
		}
	}
	if s.Len() != 4 {
		t.Fatalf("Len = %d, want 4", s.Len())
	}
	for _, key := range []string{"k46", "k47", "k48", "k49"} {
		if _, ok := lookup(s, key, at(50*time.Second)); !ok {
			t.Errorf("%s was evicted, want the four newest retained", key)
		}
	}
}

func TestStoreDrop(t *testing.T) {
	s := NewStore(time.Hour, 8)
	bind(s, "k1", "auth-1", t0)
	bind(s, "k2", "auth-1", t0)

	s.Drop("claude", "opus", "k1")
	s.Drop("claude", "opus", "missing") // no-op

	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	if _, ok := lookup(s, "k1", t0); ok {
		t.Error("dropped binding is still reachable")
	}
	if _, ok := lookup(s, "k2", t0); !ok {
		t.Error("Drop removed the wrong binding")
	}
}

func TestStoreDropAuth(t *testing.T) {
	s := NewStore(time.Hour, 8)
	bind(s, "k1", "auth-1", t0)
	bind(s, "k2", "auth-2", t0)
	s.Bind("claude", "sonnet", "k3", "auth-1", t0)
	bind(s, "k4", "auth-1", t0)

	if n := s.DropAuth("auth-1"); n != 3 {
		t.Errorf("DropAuth = %d, want 3", n)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
	if _, ok := lookup(s, "k2", t0); !ok {
		t.Error("DropAuth removed a binding on another credential")
	}
	if n := s.DropAuth("auth-1"); n != 0 {
		t.Errorf("second DropAuth = %d, want 0", n)
	}
	if n := s.DropAuth("unknown"); n != 0 {
		t.Errorf("DropAuth on an unknown credential = %d, want 0", n)
	}
}

func TestStoreCountByAuth(t *testing.T) {
	s := NewStore(time.Hour, 8)
	bind(s, "k1", "auth-1", t0)
	bind(s, "k2", "auth-1", t0)
	s.Bind("claude", "sonnet", "k1", "auth-1", t0)
	bind(s, "k3", "auth-2", t0)

	want := map[string]int{"auth-1": 3, "auth-2": 1}
	if got := s.CountByAuth(); !reflect.DeepEqual(got, want) {
		t.Errorf("CountByAuth = %v, want %v", got, want)
	}

	s.DropAuth("auth-1")
	if got := s.CountByAuth(); !reflect.DeepEqual(got, map[string]int{"auth-2": 1}) {
		t.Errorf("CountByAuth after DropAuth = %v", got)
	}
	if got := NewStore(time.Hour, 8).CountByAuth(); len(got) != 0 {
		t.Errorf("CountByAuth on an empty store = %v, want empty", got)
	}
}

// TestStoreCountByAuthTracksEveryRemovalPath walks every way a binding leaves
// the table, because the tally is maintained as entries move rather than
// recomputed on demand.
func TestStoreCountByAuthTracksEveryRemovalPath(t *testing.T) {
	s := NewStore(10*time.Minute, 3)
	scan := func() map[string]int {
		t.Helper()
		counts := make(map[string]int)
		for _, b := range s.All() {
			counts[b.AuthID]++
		}
		return counts
	}
	check := func(step string) {
		t.Helper()
		if got, want := s.CountByAuth(), scan(); !reflect.DeepEqual(got, want) {
			t.Errorf("after %s CountByAuth = %v, want %v", step, got, want)
		}
	}

	bind(s, "k1", "auth-1", t0)
	bind(s, "k2", "auth-1", t0)
	check("bind")

	// Rebinding to another credential moves the entry rather than adding one.
	bind(s, "k2", "auth-2", t0)
	check("rebind")
	if got := s.CountByAuth()["auth-1"]; got != 1 {
		t.Errorf("auth-1 holds %d bindings after a rebind away from it, want 1", got)
	}

	// Over the cap, so the least recently seen goes.
	bind(s, "k3", "auth-2", at(time.Minute))
	bind(s, "k4", "auth-2", at(2*time.Minute))
	check("eviction")
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want the cap", s.Len())
	}

	// A lookup past the TTL drops the entry it touches.
	lookup(s, "k2", at(time.Hour))
	check("expiry on lookup")

	s.Drop("claude", "opus", "k3")
	check("Drop")
	s.DropAuth("auth-2")
	check("DropAuth")
	s.Sweep(at(2 * time.Hour))
	check("Sweep")
	if got := s.CountByAuth(); len(got) != 0 {
		t.Errorf("CountByAuth on an emptied store = %v, want empty", got)
	}
}

func TestStoreSweep(t *testing.T) {
	s := NewStore(10*time.Minute, 8)
	bind(s, "stale-1", "auth-1", t0)
	bind(s, "stale-2", "auth-1", at(time.Minute))
	bind(s, "fresh", "auth-2", at(20*time.Minute))

	if n := s.Sweep(at(15 * time.Minute)); n != 2 {
		t.Errorf("Sweep = %d, want 2", n)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
	if _, ok := lookup(s, "fresh", at(20*time.Minute)); !ok {
		t.Error("Sweep removed a live binding")
	}
	if n := s.Sweep(at(15 * time.Minute)); n != 0 {
		t.Errorf("second Sweep = %d, want 0", n)
	}
	if n := s.Sweep(at(48 * time.Hour)); n != 1 {
		t.Errorf("Sweep past every TTL = %d, want 1", n)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
}

func TestStoreAllIsDeterministic(t *testing.T) {
	s := NewStore(time.Hour, 8)
	bind(s, "newest", "auth-1", at(3*time.Minute))
	bind(s, "middle", "auth-2", at(2*time.Minute))
	bind(s, "oldest", "auth-3", at(time.Minute))
	// Ties break on the key so the table renders identically every time.
	bind(s, "tie-b", "auth-4", at(3*time.Minute))
	bind(s, "tie-a", "auth-5", at(3*time.Minute))
	// One conversation on two models is two rows, ordered by model.
	s.Bind("claude", "sonnet", "newest", "auth-6", at(3*time.Minute))

	want := []string{"newest/opus", "newest/sonnet", "tie-a/opus", "tie-b/opus", "middle/opus", "oldest/opus"}
	for i := range 5 {
		all := s.All()
		got := make([]string, 0, len(all))
		for _, b := range all {
			got = append(got, b.SessionKey+"/"+b.Model)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("All (pass %d) = %v, want %v", i, got, want)
		}
	}
	if len(s.All()) != s.Len() {
		t.Errorf("All returned %d bindings, Len = %d", len(s.All()), s.Len())
	}
}

func TestStoreAllIsACopy(t *testing.T) {
	s := NewStore(time.Hour, 8)
	bind(s, "k1", "auth-1", t0)

	all := s.All()
	all[0].AuthID = "tampered"

	if got, _ := lookup(s, "k1", t0); got.AuthID != "auth-1" {
		t.Errorf("AuthID = %q, want the store's own state untouched", got.AuthID)
	}
}

func TestStoreEmpty(t *testing.T) {
	s := NewStore(time.Hour, 8)
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
	if all := s.All(); len(all) != 0 {
		t.Errorf("All = %v, want empty", all)
	}
	if n := s.Sweep(t0); n != 0 {
		t.Errorf("Sweep = %d, want 0", n)
	}
	if n := s.DropAuth("auth-1"); n != 0 {
		t.Errorf("DropAuth = %d, want 0", n)
	}
	s.Drop("claude", "opus", "k1")
}

// A non-positive TTL or cap disables that bound, which model.Config.Normalize
// never produces but the store must survive.
func TestStoreUnbounded(t *testing.T) {
	s := NewStore(0, 0)
	for i := range 100 {
		bind(s, fmt.Sprintf("k%d", i), "auth-1", t0)
	}
	if s.Len() != 100 {
		t.Errorf("Len = %d, want 100 with the cap disabled", s.Len())
	}
	if _, ok := lookup(s, "k0", at(365*24*time.Hour)); !ok {
		t.Error("Lookup expired a binding with the TTL disabled")
	}
	if n := s.Sweep(at(365 * 24 * time.Hour)); n != 0 {
		t.Errorf("Sweep = %d, want 0 with the TTL disabled", n)
	}
}

// The pick path runs with no serialization from the host.
func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore(time.Minute, 64)
	const workers = 16
	const iterations = 200

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range iterations {
				now := at(time.Duration(i) * time.Second)
				key := fmt.Sprintf("k%d", i%32)
				auth := fmt.Sprintf("auth-%d", w%4)
				switch i % 8 {
				case 0:
					bind(s, key, auth, now)
				case 1:
					lookup(s, key, now)
				case 2:
					s.All()
				case 3:
					s.Sweep(now)
				case 4:
					s.Drop("claude", "opus", key)
				case 5:
					s.DropAuth(auth)
				case 6:
					if counts := s.CountByAuth(); len(counts) > 4 {
						panic(fmt.Sprintf("CountByAuth named %d credentials, want at most the 4 in play", len(counts)))
					}
				default:
					s.Len()
				}
			}
		}(w)
	}
	wg.Wait()

	if got := s.Len(); got < 0 || got > 64 {
		t.Errorf("Len = %d, want 0..64", got)
	}
	if len(s.All()) != s.Len() {
		t.Errorf("All returned %d bindings, Len = %d", len(s.All()), s.Len())
	}
}
