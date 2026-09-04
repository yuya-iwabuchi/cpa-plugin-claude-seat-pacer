package quota

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

func sessionWindow(utilization float64) model.Window {
	return model.Window{
		Kind:        model.WindowSession,
		Utilization: utilization,
		ResetsAt:    at(21, 0),
		Duration:    model.SessionDuration,
	}
}

func weeklyWindow(utilization float64) model.Window {
	return model.Window{
		Kind:        model.WindowWeekly,
		Utilization: utilization,
		ResetsAt:    day(10, 14),
		Duration:    model.WeeklyDuration,
	}
}

func fableWindow(utilization float64) model.Window {
	return model.Window{
		Kind:        model.WindowWeeklyScoped,
		Scope:       "Fable",
		Utilization: utilization,
		ResetsAt:    day(10, 14),
		Duration:    model.WeeklyDuration,
	}
}

func endpointSnapshot(authID string, observedAt time.Time, windows ...model.Window) model.AuthSnapshot {
	return model.AuthSnapshot{
		AuthID:     authID,
		AuthIndex:  "0",
		Label:      authID + " label",
		Windows:    windows,
		ObservedAt: observedAt,
		Source:     model.SourceUsageEndpoint,
	}
}

func mustGet(t *testing.T, s *Store, authID string) model.AuthSnapshot {
	t.Helper()
	snap, ok := s.Get(authID)
	if !ok {
		t.Fatalf("no snapshot for %q", authID)
	}
	return snap
}

func utilizationOf(t *testing.T, snap model.AuthSnapshot, kind model.WindowKind, scope string) float64 {
	t.Helper()
	w, ok := snap.Window(kind, scope)
	if !ok {
		t.Fatalf("no %s/%q window in %+v", kind, scope, snap.Windows)
	}
	return w.Utilization
}

func TestStorePutAndGet(t *testing.T) {
	s := NewStore()
	if _, ok := s.Get("missing"); ok {
		t.Error("Get reported a snapshot for an unknown credential")
	}

	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5), weeklyWindow(0.1)))
	snap := mustGet(t, s, "auth-1")
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v, want 2", snap.Windows)
	}
	if snap.Source != model.SourceUsageEndpoint || !snap.ObservedAt.Equal(testNow) {
		t.Errorf("snapshot = %+v", snap)
	}

	s.Put(model.AuthSnapshot{ObservedAt: testNow, Windows: []model.Window{sessionWindow(0.9)}})
	if len(s.All()) != 1 {
		t.Errorf("a snapshot with no auth id was stored: %+v", s.All())
	}
}

func TestStorePutReplacesTheWindowSetWholesale(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5), weeklyWindow(0.1), fableWindow(0.2)))

	later := testNow.Add(2 * time.Minute)
	s.Put(endpointSnapshot("auth-1", later, sessionWindow(0.6)))

	snap := mustGet(t, s, "auth-1")
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %+v, want only the session window", snap.Windows)
	}
	if got := utilizationOf(t, snap, model.WindowSession, ""); got != 0.6 {
		t.Errorf("session utilization = %v, want 0.6", got)
	}
	if !snap.ObservedAt.Equal(later) {
		t.Errorf("observed_at = %v, want %v", snap.ObservedAt, later)
	}
}

func TestStorePutWithAnErrorRetainsPriorReadings(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5), weeklyWindow(0.1)))

	failed := model.AuthSnapshot{
		AuthID:     "auth-1",
		ObservedAt: testNow.Add(2 * time.Minute),
		Source:     model.SourceUsageEndpoint,
		Err:        "quota: transport: request failed",
	}
	s.Put(failed)

	snap := mustGet(t, s, "auth-1")
	if snap.Err != failed.Err {
		t.Errorf("err = %q, want %q", snap.Err, failed.Err)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("windows = %+v, want the prior two", snap.Windows)
	}
	if got := utilizationOf(t, snap, model.WindowSession, ""); got != 0.5 {
		t.Errorf("session utilization = %v, want the prior 0.5", got)
	}
	if !snap.ObservedAt.Equal(testNow) {
		t.Errorf("observed_at = %v, want the prior %v: a failure observes nothing", snap.ObservedAt, testNow)
	}
	if snap.Label != "auth-1 label" {
		t.Errorf("label = %q, want the prior label", snap.Label)
	}

	// A later success clears the error and re-establishes the window set.
	s.Put(endpointSnapshot("auth-1", testNow.Add(4*time.Minute), sessionWindow(0.7)))
	if snap := mustGet(t, s, "auth-1"); snap.Err != "" {
		t.Errorf("err = %q, want cleared by a successful read", snap.Err)
	}
}

func TestStorePutWithAnErrorForAnUnknownCredential(t *testing.T) {
	s := NewStore()
	s.Put(model.AuthSnapshot{
		AuthID:     "auth-1",
		ObservedAt: testNow,
		Source:     model.SourceUsageEndpoint,
		Err:        "quota: auth (http 401): credential rejected",
	})

	snap := mustGet(t, s, "auth-1")
	if snap.Err == "" {
		t.Error("err was dropped, so a credential that never read would look absent")
	}
	if len(snap.Windows) != 0 {
		t.Errorf("windows = %+v, want none", snap.Windows)
	}
}

func TestStorePutInheritsIdentityFromThePriorEntry(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5)))
	s.Put(model.AuthSnapshot{
		AuthID:     "auth-1",
		ObservedAt: testNow.Add(time.Minute),
		Source:     model.SourceUsageEndpoint,
		Windows:    []model.Window{sessionWindow(0.6)},
	})

	snap := mustGet(t, s, "auth-1")
	if snap.AuthIndex != "0" || snap.Label != "auth-1 label" {
		t.Errorf("identity = (%q,%q), want it carried over", snap.AuthIndex, snap.Label)
	}
}

func TestStoreMergeHeadersLeavesUntouchedWindowsIntact(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5), weeklyWindow(0.1), fableWindow(0.2)))

	observed := testNow.Add(90 * time.Second)
	s.MergeHeaders("auth-1", []model.Window{{
		Kind:        model.WindowSession,
		Utilization: 0.72,
		ResetsAt:    at(21, 0),
		Duration:    model.SessionDuration,
		Status:      model.StatusAllowedWarning,
	}}, observed)

	snap := mustGet(t, s, "auth-1")
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %+v, want all three retained", snap.Windows)
	}
	if got := utilizationOf(t, snap, model.WindowSession, ""); got != 0.72 {
		t.Errorf("session utilization = %v, want the merged 0.72", got)
	}
	if got := utilizationOf(t, snap, model.WindowWeekly, ""); got != 0.1 {
		t.Errorf("weekly utilization = %v, want the endpoint's 0.1", got)
	}
	if got := utilizationOf(t, snap, model.WindowWeeklyScoped, "Fable"); got != 0.2 {
		t.Errorf("fable utilization = %v, want the endpoint's 0.2", got)
	}
	if session, _ := snap.Window(model.WindowSession, ""); session.Status != model.StatusAllowedWarning {
		t.Errorf("session status = %q, want the merged one", session.Status)
	}
	if !snap.ObservedAt.Equal(observed) {
		t.Errorf("observed_at = %v, want it advanced to %v", snap.ObservedAt, observed)
	}
	if snap.Source != model.SourceUsageEndpoint {
		t.Errorf("source = %q, want the endpoint read that still covers the other windows", snap.Source)
	}
}

func TestStoreMergeHeadersAppendsAnUnseenWindow(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5), weeklyWindow(0.1)))
	s.MergeHeaders("auth-1", []model.Window{fableWindow(0.44)}, testNow.Add(time.Minute))

	snap := mustGet(t, s, "auth-1")
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %+v, want the merged one appended", snap.Windows)
	}
	if got := utilizationOf(t, snap, model.WindowWeeklyScoped, "Fable"); got != 0.44 {
		t.Errorf("fable utilization = %v, want 0.44", got)
	}
}

func TestStoreMergeHeadersCreatesAnEntryForAnUnknownCredential(t *testing.T) {
	s := NewStore()
	s.MergeHeaders("auth-9", []model.Window{sessionWindow(0.3)}, testNow)

	snap := mustGet(t, s, "auth-9")
	if snap.Source != model.SourceResponseHeaders {
		t.Errorf("source = %q, want %q", snap.Source, model.SourceResponseHeaders)
	}
	if !snap.ObservedAt.Equal(testNow) {
		t.Errorf("observed_at = %v, want %v", snap.ObservedAt, testNow)
	}
	if got := utilizationOf(t, snap, model.WindowSession, ""); got != 0.3 {
		t.Errorf("session utilization = %v, want 0.3", got)
	}
}

func TestStoreMergeHeadersDropsAStaleObservation(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5)))

	fresh := testNow.Add(2 * time.Minute)
	s.MergeHeaders("auth-1", []model.Window{sessionWindow(0.8)}, fresh)
	s.MergeHeaders("auth-1", []model.Window{sessionWindow(0.1)}, testNow.Add(time.Minute))

	snap := mustGet(t, s, "auth-1")
	if got := utilizationOf(t, snap, model.WindowSession, ""); got != 0.8 {
		t.Errorf("session utilization = %v, want the newer 0.8", got)
	}
	if !snap.ObservedAt.Equal(fresh) {
		t.Errorf("observed_at = %v, want %v", snap.ObservedAt, fresh)
	}
}

func TestStoreMergeHeadersIgnoresEmptyInput(t *testing.T) {
	s := NewStore()
	s.MergeHeaders("", []model.Window{sessionWindow(0.3)}, testNow)
	s.MergeHeaders("auth-1", nil, testNow)
	if got := s.All(); len(got) != 0 {
		t.Errorf("All = %+v, want empty", got)
	}
}

func TestStoreHandsBackCopies(t *testing.T) {
	s := NewStore()
	s.Put(endpointSnapshot("auth-1", testNow, sessionWindow(0.5)))

	snap := mustGet(t, s, "auth-1")
	snap.Windows[0].Utilization = 99
	if got := utilizationOf(t, mustGet(t, s, "auth-1"), model.WindowSession, ""); got != 0.5 {
		t.Errorf("session utilization = %v, want the stored 0.5", got)
	}

	merged := []model.Window{fableWindow(0.2)}
	s.MergeHeaders("auth-1", merged, testNow.Add(time.Minute))
	merged[0].Utilization = 99
	if got := utilizationOf(t, mustGet(t, s, "auth-1"), model.WindowWeeklyScoped, "Fable"); got != 0.2 {
		t.Errorf("fable utilization = %v, want the stored 0.2", got)
	}
}

func TestStoreAllIsOrderedByAuthID(t *testing.T) {
	s := NewStore()
	for _, id := range []string{"delta", "alpha", "charlie", "bravo"} {
		s.Put(endpointSnapshot(id, testNow, sessionWindow(0.1)))
	}

	want := []string{"alpha", "bravo", "charlie", "delta"}
	for attempt := 0; attempt < 5; attempt++ {
		got := s.All()
		if len(got) != len(want) {
			t.Fatalf("All returned %d snapshots, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].AuthID != want[i] {
				t.Fatalf("All()[%d] = %q, want %q", i, got[i].AuthID, want[i])
			}
		}
	}
}

func TestStorePrune(t *testing.T) {
	s := NewStore()
	for _, id := range []string{"auth-1", "auth-2", "auth-3"} {
		s.Put(endpointSnapshot(id, testNow, sessionWindow(0.1)))
	}

	s.Prune(map[string]struct{}{"auth-1": {}, "auth-3": {}})
	got := s.All()
	if len(got) != 2 || got[0].AuthID != "auth-1" || got[1].AuthID != "auth-3" {
		t.Fatalf("All = %+v, want auth-1 and auth-3", got)
	}

	s.Prune(nil)
	if got := s.All(); len(got) != 0 {
		t.Errorf("All = %+v, want empty after pruning to nothing", got)
	}
}

// TestStoreConcurrentAccess is the -race target: the pick path reads while the
// poll loop writes and the host's credential list changes underneath both.
func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore()
	ids := []string{"auth-1", "auth-2", "auth-3", "auth-4"}
	keep := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		keep[id] = struct{}{}
	}

	const rounds = 200
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(4)

		go func(id string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				observed := testNow.Add(time.Duration(i) * time.Second)
				s.Put(endpointSnapshot(id, observed, sessionWindow(float64(i%100)/100), weeklyWindow(0.1)))
				if i%7 == 0 {
					s.Put(model.AuthSnapshot{
						AuthID:     id,
						ObservedAt: observed,
						Source:     model.SourceUsageEndpoint,
						Err:        fmt.Sprintf("quota: transport: round %d", i),
					})
				}
			}
		}(id)

		go func(id string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				s.MergeHeaders(id, []model.Window{
					sessionWindow(float64(i%50) / 100),
					fableWindow(float64(i%25) / 100),
				}, testNow.Add(time.Duration(i)*time.Second))
			}
		}(id)

		go func(id string) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if snap, ok := s.Get(id); ok {
					for _, w := range snap.Windows {
						_ = w.Elapsed(testNow)
					}
				}
			}
		}(id)

		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				for _, snap := range s.All() {
					_ = snap.Stale(testNow, time.Minute)
				}
				if i%13 == 0 {
					s.Prune(keep)
				}
			}
		}()
	}
	wg.Wait()

	if got := s.All(); len(got) != len(ids) {
		t.Fatalf("All returned %d snapshots, want %d", len(got), len(ids))
	}
}
