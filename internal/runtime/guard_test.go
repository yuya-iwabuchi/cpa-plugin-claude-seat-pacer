package runtime

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestGuardContainsAPanicAndLogsIt covers the poll loop's guard: a panic in the
// guarded work is recovered, reported to the caller, and logged with what
// panicked.
func TestGuardContainsAPanicAndLogsIt(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	if tp.guard("probe", func() {}) {
		t.Error("guard reported a panic for work that returned")
	}
	if !tp.guard("probe", func() { panic("boom") }) {
		t.Fatal("guard did not report the panic")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range tp.host.logs() {
			if strings.Contains(l.Message, "recovered from a panic") && fmt.Sprint(l.Fields["in"]) == "probe" && fmt.Sprint(l.Fields["panic"]) == "boom" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the recovered panic was not logged: %+v", tp.host.logs())
}

// TestSpawnContainsAPanic covers host.spawn: a panic in the work it runs stays
// on that goroutine. Unguarded, it would take down the test binary.
func TestSpawnContainsAPanic(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	ran := make(chan struct{})
	tp.Plugin.host.spawn(func() {
		defer close(ran)
		panic("boom")
	})
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("spawned work never ran")
	}
	tp.Plugin.host.drain(2 * time.Second)
}
