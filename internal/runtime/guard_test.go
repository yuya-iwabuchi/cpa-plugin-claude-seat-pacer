package runtime

import (
	"context"
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

// TestInvokeCtxReportsACallbackPanic covers a host callback that panics: the
// caller gets the panic as the call's error at once, rather than waiting out
// its deadline.
func TestInvokeCtxReportsACallbackPanic(t *testing.T) {
	hst := newHost(func(string, []byte) ([]byte, error) { panic("boom") })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := hst.invokeCtx(ctx, MethodHostLog, HostLogRequest{Level: "debug"}, nil)
	if err == nil || !strings.Contains(err.Error(), "panicked: boom") {
		t.Fatalf("invokeCtx error = %v, want the panic", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("invokeCtx returned after %v; a panic must not wait out the deadline", waited)
	}
	hst.drain(2 * time.Second)
}

// TestGuardedPollContainsAPanicAndReschedules covers the poll loop's step: a
// poll that panics is recovered, the next poll waits a regular interval, and
// nextPollAt names that wake.
func TestGuardedPollContainsAPanicAndReschedules(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	panicked := false
	tp.Plugin.now = func() time.Time {
		if !panicked {
			panicked = true
			panic("boom")
		}
		return testNow
	}
	wait := tp.guardedPoll(context.Background())
	if !panicked {
		t.Fatal("the poll never reached the panic")
	}
	if want := tp.config().Quota.PollInterval; wait != want {
		t.Errorf("wait after a panicked poll = %v, want the poll interval %v", wait, want)
	}
	tp.mu.Lock()
	next := tp.nextPollAt
	tp.mu.Unlock()
	if want := testNow.Add(wait); !next.Equal(want) {
		t.Errorf("nextPollAt = %v, want %v", next, want)
	}
}
