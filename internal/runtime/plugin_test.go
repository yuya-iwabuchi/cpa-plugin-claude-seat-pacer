package runtime

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRegisterThenReconfigureIsIdempotent(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)

	res := tp.register(t, MethodPluginRegister, testConfigYAML)
	if res.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", res.SchemaVersion, SchemaVersion)
	}
	if res.Metadata.Name == "" || res.Metadata.Version == "" || res.Metadata.Author == "" || res.Metadata.GitHubRepository == "" {
		t.Errorf("metadata has a blank mandatory field: %+v", res.Metadata)
	}
	for _, cap := range []string{CapabilityRequestInterceptor, CapabilityScheduler, CapabilityUsagePlugin, CapabilityManagementAPI} {
		if !res.Capabilities[cap] {
			t.Errorf("capability %s not declared", cap)
		}
	}

	tp.lifeMu.Lock()
	first := tp.poller
	tp.lifeMu.Unlock()
	builds := tp.built

	// Reconfigure with a different pace knob but the same affinity settings:
	// the config swaps, the poller and binding table stay.
	tp.register(t, MethodPluginReconfigure, testConfigYAML+"pace:\n  hard-cutoff: 0.9\n")
	if got := tp.config().Pace.HardCutoff; got != 0.9 {
		t.Errorf("hard-cutoff = %v after reconfigure, want 0.9", got)
	}
	tp.lifeMu.Lock()
	second := tp.poller
	tp.lifeMu.Unlock()
	if first == nil || first != second {
		t.Error("reconfigure restarted the poller")
	}
	if tp.built != builds {
		t.Errorf("binding store rebuilt %d time(s) on a reconfigure that kept affinity settings", tp.built-builds)
	}

	// A TTL change rebuilds the table.
	tp.register(t, MethodPluginReconfigure, testConfigYAML+"affinity:\n  ttl: 30m\n")
	if tp.built != builds+1 {
		t.Errorf("binding store builds = %d, want %d after a TTL change", tp.built, builds+1)
	}
	if got := tp.config().Affinity.TTL; got != 30*time.Minute {
		t.Errorf("affinity TTL = %v, want 30m", got)
	}
	if got := tp.config().Pace.HardCutoff; got != 0.98 {
		t.Errorf("hard-cutoff = %v, want the default restored when the key is absent", got)
	}
}

func TestInvalidConfigRegistersInert(t *testing.T) {
	tp := newTestPlugin(t, "enabled: true\npace: [not a map\n")
	if tp.config().Enabled {
		t.Error("an unparsable config left the plugin enabled")
	}
	logs := tp.host.logs()
	if len(logs) == 0 || logs[0].Level != "warn" {
		t.Errorf("no warn log for the bad config: %+v", logs)
	}
	// The plugin still declines cleanly rather than erroring.
	resp := tp.pick(t, pickRequest(fableModel, "k", "seat-a", "seat-b"))
	if resp.Handled {
		t.Error("a disabled plugin handled a pick")
	}
}

func TestDurationsDecodeFromStrings(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML+"quota:\n  poll-interval: 5m\n  max-staleness: 20m\naffinity:\n  ttl: 2h\n")
	cfg := tp.config()
	if cfg.Quota.PollInterval != 5*time.Minute || cfg.Quota.MaxStaleness != 20*time.Minute || cfg.Affinity.TTL != 2*time.Hour {
		t.Errorf("durations = %v %v %v", cfg.Quota.PollInterval, cfg.Quota.MaxStaleness, cfg.Affinity.TTL)
	}
}

func TestQuiesceStopsPollerAndIsIdempotent(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.callOK(t, MethodPluginQuiesce, nil, nil)
	tp.lifeMu.Lock()
	stopped := tp.poller == nil
	tp.lifeMu.Unlock()
	if !stopped {
		t.Fatal("quiesce left the poller running")
	}
	tp.callOK(t, MethodPluginShutdown, nil, nil)
	// A register after quiesce brings the poller back.
	tp.register(t, MethodPluginRegister, testConfigYAML)
	tp.lifeMu.Lock()
	restarted := tp.poller != nil
	tp.lifeMu.Unlock()
	if !restarted {
		t.Error("register after quiesce did not restart the poller")
	}
}

func TestPanicOnALifecycleMethodIsAnErrorEnvelope(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.handle = func(string, []byte) ([]byte, error) { panic("boom") }

	raw, ok := tp.Call(MethodPluginRegister, []byte(`{}`))
	if ok {
		t.Fatal("a panicking handler reported success")
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not JSON: %s", raw)
	}
	if env.OK || env.Error == nil || env.Error.Code != codePluginPanic || !strings.Contains(env.Error.Message, "boom") {
		t.Errorf("envelope = %s, want a plugin_panic error mentioning boom", raw)
	}
}

// TestPanicOnATrafficMethodDegradesRatherThanFailing covers the methods the
// host routes live traffic through: an error envelope from scheduler.pick
// hard-fails the request with no fallback to the host's own selector, one from
// an interceptor makes the host drop the response and leave the bridge headers
// unclear, and one from management.handle becomes a 502.
func TestPanicOnATrafficMethodDegradesRatherThanFailing(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.handle = func(string, []byte) ([]byte, error) { panic("boom") }

	var pick SchedulerPickResponse
	tp.callOK(t, MethodSchedulerPick, mustJSON(t, pickRequest(fableModel, "k", "seat-a", "seat-b")), &pick)
	if pick.Handled || pick.AuthID != "" {
		t.Errorf("pick = %+v, want a decline", pick)
	}

	var intercept RequestInterceptResponse
	tp.callOK(t, MethodRequestInterceptBefore, interceptPayload(t, claudeCodeBody("s")), &intercept)
	if len(intercept.Headers) != 0 {
		t.Errorf("intercept_before = %+v, want no headers set", intercept)
	}
	assertClearsBridge(t, intercept)

	for _, method := range []string{MethodRequestInterceptAfter, MethodUsageHandle} {
		var out map[string]any
		tp.callOK(t, method, []byte(`{}`), &out)
		if len(out) != 0 {
			t.Errorf("%s = %v, want {}", method, out)
		}
	}

	var mgmt ManagementResponse
	tp.callOK(t, MethodManagementHandle, []byte(`{}`), &mgmt)
	if mgmt.StatusCode != http.StatusInternalServerError {
		t.Errorf("management.handle = %d, want 500", mgmt.StatusCode)
	}
	if strings.Contains(string(mgmt.Body), "boom") {
		t.Errorf("management body names the panic: %s", mgmt.Body)
	}
}

func TestUnknownMethodIsAnUnknownMethodEnvelope(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	raw, ok := tp.Call("auth.login_start", nil)
	if ok {
		t.Fatal("unknown method reported success")
	}
	var env Envelope
	_ = json.Unmarshal(raw, &env)
	if env.Error == nil || env.Error.Code != codeUnknownMethod {
		t.Errorf("envelope = %s, want unknown_method", raw)
	}
}

func TestInterceptAfterIsEmpty(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	var out map[string]any
	tp.callOK(t, MethodRequestInterceptAfter, []byte(`{"RequestID":"x"}`), &out)
	if len(out) != 0 {
		t.Errorf("intercept_after = %v, want {}", out)
	}
}
