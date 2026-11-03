package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pluginAt registers a plugin whose history file is path, with the real
// binding store, so two plugins sharing a path stand for one build and the
// build replacing it.
func pluginAt(t *testing.T, path, configYAML string) *testPlugin {
	t.Helper()
	tp := seededPlugin(t, path, configYAML)
	tp.awaitStartupLoad(t)
	return tp
}

// A build saves its live bindings when the host replaces it or shuts it down,
// and the next build starts with them, so a plugin update or a host restart
// keeps each conversation on its seat.
func TestBindingsSurviveAReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, tp *testPlugin)
	}{
		{"quiesce", func(t *testing.T, tp *testPlugin) { tp.callOK(t, MethodPluginQuiesce, nil, nil) }},
		{"shutdown", func(t *testing.T, tp *testPlugin) { tp.Shutdown() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.json")
			old := pluginAt(t, path, testConfigYAML)
			store := old.bindingStore()
			store.Bind("claude", "opus", "live", "seat-a", testNow.Add(-10*time.Minute))
			store.Lookup("claude", "opus", "live", testNow.Add(-5*time.Minute))
			store.Bind("claude", "opus", "idle", "seat-b", testNow.Add(-2*time.Hour))
			tc.stop(t, old)

			body, err := os.ReadFile(filepath.Join(filepath.Dir(path), bindingsFileName))
			var saved bindingsFile
			if err == nil {
				err = json.Unmarshal(body, &saved)
			}
			if err != nil || len(saved.Bindings) != 1 {
				t.Fatalf("bindings file = %s (%v), want only the live binding", body, err)
			}

			next := pluginAt(t, path, testConfigYAML)
			got := next.bindingStore().All()
			if len(got) != 1 {
				t.Fatalf("restored %+v, want only the live binding", got)
			}
			b := got[0]
			if b.SessionKey != "live" || b.AuthID != "seat-a" || b.Hits != 1 ||
				!b.BoundAt.Equal(testNow.Add(-10*time.Minute)) || !b.LastSeen.Equal(testNow.Add(-5*time.Minute)) {
				t.Errorf("restored %+v, want live on seat-a as it was saved", b)
			}
		})
	}
}

// With affinity.persist off a build neither reads the bindings file nor keeps
// one: the stop that follows deletes what an earlier build saved.
func TestPersistOffDeletesTheBindingsFile(t *testing.T) {
	const off = testConfigYAML + "affinity:\n  persist: false\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")

	on := pluginAt(t, path, testConfigYAML)
	on.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	on.callOK(t, MethodPluginQuiesce, nil, nil)

	tp := pluginAt(t, path, off)
	if tp.bindingStore().Len() != 0 {
		t.Errorf("restored %+v with persist off", tp.bindingStore().All())
	}
	tp.callOK(t, MethodPluginQuiesce, nil, nil)
	if _, err := os.Stat(filepath.Join(dir, bindingsFileName)); !os.IsNotExist(err) {
		t.Errorf("bindings file after a stop with persist off: %v", err)
	}
}

// A config block that does not parse leaves the plugin inert under the
// defaults, which keep bindings on disk; an operator who turned persistence
// off still gets no file.
func TestAnUnparsableConfigWritesNoBindingsFile(t *testing.T) {
	const off = testConfigYAML + "affinity:\n  persist: false\n"
	dir := t.TempDir()
	tp := pluginAt(t, filepath.Join(dir, "history.json"), off)
	tp.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	payload, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte("affinity: 3\n"), SchemaVersion: 4})
	tp.Call(MethodPluginReconfigure, payload)
	tp.Shutdown()
	if _, err := os.Stat(filepath.Join(dir, bindingsFileName)); !os.IsNotExist(err) {
		t.Errorf("bindings file after a stop under an unparsable config: %v", err)
	}
}

// The host can shut a build down before its register, as when it abandons a
// load; that build has no table and leaves the saved one in place.
func TestAShutdownBeforeRegisterKeepsTheBindingsFile(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, bindingsFileName)
	if err := os.WriteFile(state, []byte(`{"version":1,"bindings":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(Options{Name: "claude-seat-pacer", Version: "0", Host: newFakeHost().call, HistoryFile: filepath.Join(dir, "history.json"), Now: func() time.Time { return testNow }})
	p.Shutdown()
	if _, err := os.Stat(state); err != nil {
		t.Errorf("bindings file after a shutdown with no register: %v", err)
	}
}

// boundSeats maps each conversation key in tp's table to its credential.
func boundSeats(tp *testPlugin) map[string]string {
	out := make(map[string]string)
	for _, b := range tp.bindingStore().All() {
		out[b.SessionKey] = b.AuthID
	}
	return out
}

// The host shuts a replaced build down only after the build that replaced it,
// so the replaced build's shutdown must not write its stale table over the
// live one.
func TestAReplacedBuildLeavesTheLiveTableAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	old := pluginAt(t, path, testConfigYAML)
	old.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	old.callOK(t, MethodPluginQuiesce, nil, nil)

	live := pluginAt(t, path, testConfigYAML)
	live.bindingStore().Bind("claude", "opus", "k", "seat-c", testNow)
	live.bindingStore().Bind("claude", "opus", "k2", "seat-b", testNow)
	live.Shutdown()
	old.Shutdown()

	want := map[string]string{"k": "seat-c", "k2": "seat-b"}
	if got := boundSeats(pluginAt(t, path, testConfigYAML)); !reflect.DeepEqual(got, want) {
		t.Errorf("restored %v, want the live build's %v", got, want)
	}
}

// A host rolls back a failed replacement by reconfiguring the build it
// quiesced, which is then the live build again and saves at its shutdown.
func TestARolledBackBuildSavesAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	tp := pluginAt(t, path, testConfigYAML)
	tp.callOK(t, MethodPluginQuiesce, nil, nil)
	tp.register(t, MethodPluginReconfigure, testConfigYAML)
	tp.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	tp.Shutdown()

	if got := boundSeats(pluginAt(t, path, testConfigYAML)); got["k"] != "seat-a" {
		t.Errorf("restored %v, want k on seat-a", got)
	}
}

// until polls cond until it holds, failing the test after a second.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

// A host that abandons a quiesce and rolls back can reconfigure the build
// while the quiesce is still stopping the poll loop, and the build stays live
// after both.
func TestARollbackDuringAQuiesceLeavesTheBuildLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	tp := pluginAt(t, path, testConfigYAML)
	// Holding pollMu parks the quiesce in stopPoller, at its history save.
	tp.pollMu.Lock()
	held := true
	release := func() {
		if held {
			held = false
			tp.pollMu.Unlock()
		}
	}
	t.Cleanup(release)

	quiesced := make(chan bool, 1)
	go func() {
		_, ok := tp.Call(MethodPluginQuiesce, nil)
		quiesced <- ok
	}()
	until(t, "the quiesce never reached stopPoller", func() bool {
		if tp.lifeMu.TryLock() {
			tp.lifeMu.Unlock()
			return false
		}
		return true
	})
	tp.mu.Lock()
	tp.hostSchema = 0
	tp.mu.Unlock()
	reconfigured := make(chan bool, 1)
	go func() {
		payload, _ := json.Marshal(LifecycleRequest{ConfigYAML: []byte(testConfigYAML), SchemaVersion: 4})
		_, ok := tp.Call(MethodPluginReconfigure, payload)
		reconfigured <- ok
	}()
	until(t, "the reconfigure never marked the build live", func() bool {
		tp.mu.Lock()
		defer tp.mu.Unlock()
		return tp.hostSchema != 0
	})
	release()
	if !<-quiesced || !<-reconfigured {
		t.Fatal("a lifecycle call returned an error envelope")
	}

	tp.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	tp.Shutdown()
	if got := boundSeats(pluginAt(t, path, testConfigYAML)); got["k"] != "seat-a" {
		t.Errorf("restored %v, want k on seat-a", got)
	}
}

// A host that loads a library it loaded before shares one image between the
// retired build and the reload. When it discards the reload and rolls back,
// the retired build's shutdown at exit leaves the saved table alone.
func TestAShutDownBuildDoesNotSaveAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	shared := pluginAt(t, path, testConfigYAML)
	shared.callOK(t, MethodPluginQuiesce, nil, nil)

	live := pluginAt(t, path, testConfigYAML)
	live.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	live.callOK(t, MethodPluginQuiesce, nil, nil)
	shared.register(t, MethodPluginRegister, testConfigYAML)
	shared.Shutdown()
	live.register(t, MethodPluginReconfigure, testConfigYAML)
	live.bindingStore().Bind("claude", "opus", "late", "seat-b", testNow)

	live.Shutdown()
	shared.Shutdown()
	if got, want := boundSeats(pluginAt(t, path, testConfigYAML)), map[string]string{"k": "seat-a", "late": "seat-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("restored %v, want %v", got, want)
	}
}

// A host that loads a library it loaded before reuses that image's memory, so
// a register takes the file's table over the one the image still holds: the
// file is what the build running in between saved.
func TestARegisterTakesTheSavedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	first := pluginAt(t, path, testConfigYAML)
	first.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	first.bindingStore().Bind("claude", "opus", "gone", "seat-a", testNow)
	first.callOK(t, MethodPluginQuiesce, nil, nil)

	between := pluginAt(t, path, testConfigYAML)
	between.bindingStore().DropAuth("seat-a")
	between.bindingStore().Bind("claude", "opus", "k", "seat-c", testNow)
	between.callOK(t, MethodPluginQuiesce, nil, nil)

	first.register(t, MethodPluginRegister, testConfigYAML)
	if got, want := boundSeats(first), map[string]string{"k": "seat-c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("table after the register = %v, want the saved %v", got, want)
	}

	// With no file to read, the image's own table is still not the live one.
	if err := os.Remove(filepath.Join(filepath.Dir(path), bindingsFileName)); err != nil {
		t.Fatal(err)
	}
	first.register(t, MethodPluginRegister, testConfigYAML)
	if got := boundSeats(first); len(got) != 0 {
		t.Errorf("table after a register with no file = %v, want none", got)
	}
}

// A build whose first configure is a reconfigure, as after a register call
// the host failed, still starts with the saved table and a fresh spend ledger.
func TestAFirstReconfigureTakesTheSavedTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	old := pluginAt(t, path, testConfigYAML)
	old.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	old.callOK(t, MethodPluginQuiesce, nil, nil)

	tp := &testPlugin{host: newFakeHost()}
	tp.Plugin = New(Options{Name: "claude-seat-pacer", Version: "0", Host: tp.host.call, HistoryFile: path, Now: func() time.Time { return testNow }})
	tp.startDelay = time.Hour
	t.Cleanup(tp.Shutdown)
	tp.register(t, MethodPluginReconfigure, testConfigYAML)
	if got, want := boundSeats(tp), map[string]string{"k": "seat-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("table after a first reconfigure = %v, want the saved %v", got, want)
	}
	tp.mu.Lock()
	from := tp.spendFrom
	tp.mu.Unlock()
	if !from.Equal(testNow) {
		t.Errorf("spend ledger after a first reconfigure starts at %v, want %v", from, testNow)
	}
}

// An unreadable bindings file costs only the bindings: the build registers
// with none, says so once, and its next save replaces the file.
func TestUnreadableBindingsFileIsSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	if err := os.WriteFile(filepath.Join(dir, bindingsFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	tp := pluginAt(t, path, testConfigYAML)
	if tp.bindingStore().Len() != 0 {
		t.Errorf("restored %+v from an unreadable file", tp.bindingStore().All())
	}
	warned := 0
	for _, l := range tp.host.logs() {
		if l.Level == "warn" && strings.Contains(l.Message, "bindings") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("warnings about the bindings file = %d, want 1", warned)
	}

	tp.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	tp.callOK(t, MethodPluginQuiesce, nil, nil)
	if next := pluginAt(t, path, testConfigYAML); next.bindingStore().Len() != 1 {
		t.Errorf("restored %+v after the save, want the one binding", next.bindingStore().All())
	}
}

// Only a register reads the file: a binding dropped since, such as one on a
// credential that has gone, stays dropped through a reconfigure.
func TestReconfigureLeavesTheTableAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	old := pluginAt(t, path, testConfigYAML)
	old.bindingStore().Bind("claude", "opus", "k", "seat-a", testNow)
	old.callOK(t, MethodPluginQuiesce, nil, nil)

	tp := pluginAt(t, path, testConfigYAML)
	if n := tp.bindingStore().DropAuth("seat-a"); n != 1 {
		t.Fatalf("DropAuth = %d, want the restored binding", n)
	}
	tp.register(t, MethodPluginReconfigure, testConfigYAML)
	if tp.bindingStore().Len() != 0 {
		t.Errorf("reconfigure restored %+v again", tp.bindingStore().All())
	}
}
