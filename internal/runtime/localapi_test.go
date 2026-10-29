package runtime

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/session"
)

// socketConfigYAML is the test config with the local API socket on.
const socketConfigYAML = testConfigYAML + "web:\n  socket: true\n"

// seatsGet sends a GET over the plugin's local API socket.
func seatsGet(t *testing.T, path, target string) (int, string) {
	t.Helper()
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}},
	}
	res, err := client.Get("http://seat-pacer" + target)
	if err != nil {
		t.Fatalf("GET %s over the local API socket: %v", target, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

// echoApp stands in for the status app, answering with the path it was asked.
func echoApp() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "app "+r.URL.RequestURI())
	})
}

func TestLocalAPIServesOnlyTheSeatsRoute(t *testing.T) {
	tp := newTestPlugin(t, socketConfigYAML)
	tp.SetResourceHandler(echoApp())
	path := tp.APISocketPath()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no socket at %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
	if code, body := seatsGet(t, path, "/api/seats?session=abc"); code != http.StatusOK || body != "app /api/seats?session=abc" {
		t.Errorf("seats answer = %d %q, want the app's answer", code, body)
	}
	for _, other := range []string{"/api/status", "/index.html", "/"} {
		if code, _ := seatsGet(t, path, other); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", other, code)
		}
	}
}

func TestLocalAPIClosesAtQuiesceAndShutdown(t *testing.T) {
	for _, method := range []string{MethodPluginQuiesce, MethodPluginShutdown} {
		t.Run(method, func(t *testing.T) {
			tp := newTestPlugin(t, socketConfigYAML)
			path := tp.APISocketPath()
			if _, ok := tp.Call(method, nil); !ok {
				t.Fatalf("%s failed", method)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("socket still at %s after %s (err %v)", path, method, err)
			}
			// A replacement registers again and binds the same path.
			tp.register(t, MethodPluginRegister, socketConfigYAML)
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("no socket after registering again: %v", err)
			}
		})
	}
}

func TestLocalAPIFollowsTheSwitch(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	path := tp.APISocketPath()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket bound with web.socket at its default (err %v)", err)
	}
	tp.register(t, MethodPluginReconfigure, socketConfigYAML)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no socket once web.socket is on: %v", err)
	}
	tp.register(t, MethodPluginReconfigure, testConfigYAML+"web:\n  enabled: false\n  socket: true\n")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket still bound with web.enabled off (err %v)", err)
	}
}

func TestLocalAPIReplacesAStaleSocket(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()

	gl, _, err := listenLocalAPI(path)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	_ = gl.Close()
}

func TestLocalAPILeavesALiveSocketAndOtherFilesAlone(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, _, err := listenLocalAPI(path); err == nil || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("binding over a live socket: err = %v", err)
	}

	file := shortSocketPath(t)
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenLocalAPI(file); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("binding over a regular file: err = %v", err)
	}
	if body, _ := os.ReadFile(file); string(body) != "x" {
		t.Fatal("the regular file was changed")
	}
}

func TestLocalAPIFailureIsAWarning(t *testing.T) {
	tp := newTestPlugin(t, socketConfigYAML)
	tp.Shutdown()
	tp.opts.APISocket = "/" + strings.Repeat("x", maxSocketPath) + "/api.sock"
	tp.register(t, MethodPluginRegister, socketConfigYAML)
	warnings := tp.Status(testNow, "").Warnings
	if len(warnings) == 0 || !strings.Contains(warnings[len(warnings)-1], "local API not served") {
		t.Fatalf("warnings = %v, want the socket's failure", warnings)
	}
}

func TestSessionBindingPrefersTheAskedModel(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	const id = "0b2e7c1a-4d36-4f58-9a7e-3c1f5d8e2b40"
	keys := session.ClaudeCodeKeys(id)
	store := tp.bindingStore()
	store.Bind("claude", "claude-opus-5", keys[1], "seat-a", testNow.Add(-time.Hour))
	// A side request on a smaller model, newer than the session's own.
	store.Bind("claude", "claude-haiku-5", keys[1], "seat-b", testNow)
	store.Bind("claude", "claude-opus-5", "another-conversation", "seat-c", testNow.Add(time.Minute))

	b, expires, ok := tp.SessionBinding(id, "claude-opus-5")
	if !ok || b.AuthID != "seat-a" {
		t.Errorf("SessionBinding for the session's model = %+v, %v; want seat-a", b, ok)
	}
	if ttl := tp.config().Affinity.TTL; !expires.Equal(testNow.Add(-time.Hour).Add(ttl)) {
		t.Errorf("expires at %v, want the affinity TTL after it was last seen", expires)
	}
	b, expires, ok = tp.SessionBinding(id, "claude-sonnet-5")
	if !ok || b.AuthID != "seat-b" {
		t.Errorf("SessionBinding for an unbound model = %+v, %v; want the newest, seat-b", b, ok)
	}
	if !expires.Equal(b.LastSeen) {
		t.Errorf("another model's binding expires at %v, want when it was last seen", expires)
	}
	if _, _, ok := tp.SessionBinding("00000000-0000-0000-0000-000000000000", "claude-opus-5"); ok {
		t.Error("an unbound session named a seat")
	}
	if _, _, ok := tp.SessionBinding("", ""); ok {
		t.Error("an empty session id named a seat")
	}
}

func TestLocalAPIDirectoryIsTheUsersAlone(t *testing.T) {
	path := shortSocketPath(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, _, err := listenLocalAPI(path)
	if err != nil {
		t.Fatalf("bind in a 0755 directory: %v", err)
	}
	defer ln.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700", perm)
	}

	target := t.TempDir()
	link := filepath.Join(filepath.Dir(shortSocketPath(t)), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenLocalAPI(filepath.Join(link, "api.sock")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("binding through a linked directory: err = %v", err)
	}
}

func TestLocalAPIRebindsAfterItsFileIsRemoved(t *testing.T) {
	tp := newTestPlugin(t, socketConfigYAML)
	path := tp.APISocketPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	tp.syncLocalAPI()
	if code, _ := seatsGet(t, path, "/api/status"); code != http.StatusNotFound {
		t.Fatalf("the rebound socket answered %d, want the 404 of a live server", code)
	}
}

func TestLocalAPIRebindsOverAStaleReplacement(t *testing.T) {
	tp := newTestPlugin(t, socketConfigYAML)
	path := tp.APISocketPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	tp.syncLocalAPI()
	if code, _ := seatsGet(t, path, "/api/status"); code != http.StatusNotFound {
		t.Fatalf("the rebound socket answered %d, want the 404 of a live server", code)
	}
}

func TestLocalAPILeavesASocketThatReplacedItsOwn(t *testing.T) {
	tp := newTestPlugin(t, socketConfigYAML)
	path := tp.APISocketPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	other, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// The replacement survives a poll and a shutdown, and one with no poll
	// before it.
	tp.syncLocalAPI()
	tp.Shutdown()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the other process's socket is gone after a poll and a shutdown: %v", err)
	}
	_ = conn.Close()

	_ = other.Close()
	tp.register(t, MethodPluginRegister, socketConfigYAML)
	if code, _ := seatsGet(t, path, "/api/status"); code != http.StatusNotFound {
		t.Fatalf("the plugin did not bind again: %d", code)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	another, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer another.Close()
	tp.Shutdown()
	conn, err = net.Dial("unix", path)
	if err != nil {
		t.Fatalf("the other process's socket is gone after a shutdown: %v", err)
	}
	_ = conn.Close()
}

func TestLocalAPIIsOffWithNoDataDirectory(t *testing.T) {
	tp := newTestPlugin(t, socketConfigYAML)
	tp.Shutdown()
	tp.opts.APISocket, tp.opts.HistoryFile = "", ""
	tp.register(t, MethodPluginRegister, socketConfigYAML)
	for _, w := range tp.Status(testNow, "").Warnings {
		if strings.Contains(w, "local API not served") {
			t.Fatalf("warning %q with no data directory to serve from", w)
		}
	}
}
