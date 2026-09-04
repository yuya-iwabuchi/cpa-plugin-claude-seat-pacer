package session

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func TestStoreBindAndLookup(t *testing.T) {
	s := NewStore(time.Hour, 8)

	b := s.Bind("k1", "auth-1", "opus", t0)
	if b.SessionKey != "k1" || b.AuthID != "auth-1" || b.Model != "opus" {
		t.Fatalf("Bind = %+v", b)
	}
	if !b.BoundAt.Equal(t0) || !b.LastSeen.Equal(t0) || b.Hits != 0 {
		t.Fatalf("Bind = %+v, want BoundAt and LastSeen at t0 with no hits", b)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}

	got, ok := s.Lookup("k1", at(time.Minute))
	if !ok {
		t.Fatal("Lookup missed a fresh binding")
	}
	if got.Hits != 1 || !got.LastSeen.Equal(at(time.Minute)) || !got.BoundAt.Equal(t0) {
		t.Errorf("Lookup = %+v, want one hit, refreshed LastSeen, original BoundAt", got)
	}
	if got, ok := s.Lookup("k1", at(2*time.Minute)); !ok || got.Hits != 2 {
		t.Errorf("second Lookup = %+v ok=%v, want two hits", got, ok)
	}
	if _, ok := s.Lookup("missing", t0); ok {
		t.Error("Lookup hit an unknown key")
	}
}

// The TTL bounds idle time, not total session length, so a conversation that
// keeps being used never expires.
func TestStoreTTLIsIdleTime(t *testing.T) {
	s := NewStore(10*time.Minute, 8)
	s.Bind("k1", "auth-1", "opus", t0)

	for _, d := range []time.Duration{9 * time.Minute, 18 * time.Minute, 27 * time.Minute} {
		if _, ok := s.Lookup("k1", at(d)); !ok {
			t.Fatalf("Lookup at +%v missed; TTL is bounding total length, not idle time", d)
		}
	}
	if _, ok := s.Lookup("k1", at(38*time.Minute)); ok {
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
			s.Bind("k1", "auth-1", "opus", t0)
			if _, ok := s.Lookup("k1", at(tc.idle)); ok != tc.want {
				t.Errorf("Lookup after %v = %v, want %v", tc.idle, ok, tc.want)
			}
		})
	}
}

func TestStoreRebind(t *testing.T) {
	s := NewStore(time.Hour, 8)
	s.Bind("k1", "auth-1", "opus", t0)
	s.Lookup("k1", at(time.Minute))

	// Rebinding to the same credential and model is a refresh, so the cached
	// prefix history survives.
	same := s.Bind("k1", "auth-1", "opus", at(2*time.Minute))
	if !same.BoundAt.Equal(t0) || same.Hits != 1 || !same.LastSeen.Equal(at(2*time.Minute)) {
		t.Errorf("re-Bind to the same credential = %+v, want the existing binding refreshed", same)
	}

	// A different credential is a different cached prefix.
	moved := s.Bind("k1", "auth-2", "opus", at(3*time.Minute))
	if moved.AuthID != "auth-2" || !moved.BoundAt.Equal(at(3*time.Minute)) || moved.Hits != 0 {
		t.Errorf("re-Bind to another credential = %+v, want a fresh binding", moved)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}

	// A different model on the same credential is also a different prefix.
	remodelled := s.Bind("k1", "auth-2", "sonnet", at(4*time.Minute))
	if remodelled.Model != "sonnet" || !remodelled.BoundAt.Equal(at(4*time.Minute)) {
		t.Errorf("re-Bind to another model = %+v, want a fresh binding", remodelled)
	}
}

func TestStoreLRUEviction(t *testing.T) {
	s := NewStore(time.Hour, 2)
	s.Bind("k1", "auth-1", "opus", t0)
	s.Bind("k2", "auth-2", "opus", at(time.Minute))

	// Touching k1 makes k2 the least recently seen.
	if _, ok := s.Lookup("k1", at(2*time.Minute)); !ok {
		t.Fatal("Lookup missed k1")
	}
	s.Bind("k3", "auth-3", "opus", at(3*time.Minute))

	if s.Len() != 2 {
		t.Fatalf("Len = %d, want the cap of 2", s.Len())
	}
	if _, ok := s.Lookup("k2", at(3*time.Minute)); ok {
		t.Error("k2 survived; eviction did not take the least recently seen")
	}
	for _, key := range []string{"k1", "k3"} {
		if _, ok := s.Lookup(key, at(3*time.Minute)); !ok {
			t.Errorf("%s was evicted", key)
		}
	}
}

func TestStoreEvictionHoldsTheCap(t *testing.T) {
	s := NewStore(time.Hour, 4)
	for i := range 50 {
		s.Bind(fmt.Sprintf("k%02d", i), "auth-1", "opus", at(time.Duration(i)*time.Second))
		if s.Len() > 4 {
			t.Fatalf("Len = %d after %d binds, want at most 4", s.Len(), i+1)
		}
	}
	if s.Len() != 4 {
		t.Fatalf("Len = %d, want 4", s.Len())
	}
	for _, key := range []string{"k46", "k47", "k48", "k49"} {
		if _, ok := s.Lookup(key, at(50*time.Second)); !ok {
			t.Errorf("%s was evicted, want the four newest retained", key)
		}
	}
}

func TestStoreDrop(t *testing.T) {
	s := NewStore(time.Hour, 8)
	s.Bind("k1", "auth-1", "opus", t0)
	s.Bind("k2", "auth-1", "opus", t0)

	s.Drop("k1")
	s.Drop("missing") // no-op

	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	if _, ok := s.Lookup("k1", t0); ok {
		t.Error("dropped binding is still reachable")
	}
	if _, ok := s.Lookup("k2", t0); !ok {
		t.Error("Drop removed the wrong binding")
	}
}

func TestStoreDropAuth(t *testing.T) {
	s := NewStore(time.Hour, 8)
	s.Bind("k1", "auth-1", "opus", t0)
	s.Bind("k2", "auth-2", "opus", t0)
	s.Bind("k3", "auth-1", "sonnet", t0)
	s.Bind("k4", "auth-1", "opus", t0)

	if n := s.DropAuth("auth-1"); n != 3 {
		t.Errorf("DropAuth = %d, want 3", n)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
	if _, ok := s.Lookup("k2", t0); !ok {
		t.Error("DropAuth removed a binding on another credential")
	}
	if n := s.DropAuth("auth-1"); n != 0 {
		t.Errorf("second DropAuth = %d, want 0", n)
	}
	if n := s.DropAuth("unknown"); n != 0 {
		t.Errorf("DropAuth on an unknown credential = %d, want 0", n)
	}
}

func TestStoreSweep(t *testing.T) {
	s := NewStore(10*time.Minute, 8)
	s.Bind("stale-1", "auth-1", "opus", t0)
	s.Bind("stale-2", "auth-1", "opus", at(time.Minute))
	s.Bind("fresh", "auth-2", "opus", at(20*time.Minute))

	if n := s.Sweep(at(15 * time.Minute)); n != 2 {
		t.Errorf("Sweep = %d, want 2", n)
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
	if _, ok := s.Lookup("fresh", at(20*time.Minute)); !ok {
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
	s.Bind("newest", "auth-1", "opus", at(3*time.Minute))
	s.Bind("middle", "auth-2", "opus", at(2*time.Minute))
	s.Bind("oldest", "auth-3", "opus", at(time.Minute))
	// Ties break on the key so the table renders identically every time.
	s.Bind("tie-b", "auth-4", "opus", at(3*time.Minute))
	s.Bind("tie-a", "auth-5", "opus", at(3*time.Minute))

	want := []string{"newest", "tie-a", "tie-b", "middle", "oldest"}
	for i := range 5 {
		all := s.All()
		got := make([]string, 0, len(all))
		for _, b := range all {
			got = append(got, b.SessionKey)
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
	s.Bind("k1", "auth-1", "opus", t0)

	all := s.All()
	all[0].AuthID = "tampered"

	if got, _ := s.Lookup("k1", t0); got.AuthID != "auth-1" {
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
	s.Drop("k1")
}

// A non-positive TTL or cap disables that bound, which model.Config.Normalize
// never produces but the store must survive.
func TestStoreUnbounded(t *testing.T) {
	s := NewStore(0, 0)
	for i := range 100 {
		s.Bind(fmt.Sprintf("k%d", i), "auth-1", "opus", t0)
	}
	if s.Len() != 100 {
		t.Errorf("Len = %d, want 100 with the cap disabled", s.Len())
	}
	if _, ok := s.Lookup("k0", at(365*24*time.Hour)); !ok {
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
				switch i % 7 {
				case 0:
					s.Bind(key, auth, "opus", now)
				case 1:
					s.Lookup(key, now)
				case 2:
					s.All()
				case 3:
					s.Sweep(now)
				case 4:
					s.Drop(key)
				case 5:
					s.DropAuth(auth)
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

// The store keys on whatever BindingKey composes, so one conversation on two
// models holds two bindings.
func TestStoreKeyedByBindingKey(t *testing.T) {
	s := NewStore(time.Hour, 8)
	id := Extract(nil, []byte(`{"session_id":"sess-a"}`))

	opus := BindingKey("claude", "opus", id.Key)
	sonnet := BindingKey("claude", "sonnet", id.Key)
	s.Bind(opus, "auth-1", "opus", t0)
	s.Bind(sonnet, "auth-2", "sonnet", t0)

	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	if b, _ := s.Lookup(opus, t0); b.AuthID != "auth-1" {
		t.Errorf("opus binding = %q, want auth-1", b.AuthID)
	}
	if b, _ := s.Lookup(sonnet, t0); b.AuthID != "auth-2" {
		t.Errorf("sonnet binding = %q, want auth-2", b.AuthID)
	}
}
