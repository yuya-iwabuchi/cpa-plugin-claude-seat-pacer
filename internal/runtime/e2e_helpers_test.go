package runtime_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requireHostSource is the CLIProxyAPI checkout the end-to-end tests build a
// host from. The source is not vendored, so a test that needs one skips
// without CPA_SOURCE_DIR.
//
//	CPA_SOURCE_DIR=/path/to/CLIProxyAPI GOTOOLCHAIN=auto go test ./internal/runtime -run E2E -v
func requireHostSource(t *testing.T) string {
	t.Helper()
	hostSource := strings.TrimSpace(os.Getenv("CPA_SOURCE_DIR"))
	if hostSource == "" {
		t.Skip("set CPA_SOURCE_DIR to a CLIProxyAPI checkout to run the end-to-end tests")
	}
	if _, err := os.Stat(filepath.Join(hostSource, "cmd", "server")); err != nil {
		t.Skipf("CPA_SOURCE_DIR %q has no cmd/server: %v", hostSource, err)
	}
	return hostSource
}

// libraryExtension is the shared-library suffix the host loads a plugin from,
// and serverName the host binary's file name.
func libraryExtension() (extension, serverName string) {
	switch runtime.GOOS {
	case "darwin":
		return ".dylib", "cliproxyapi"
	case "windows":
		return ".dll", "cliproxyapi.exe"
	default:
		return ".so", "cliproxyapi"
	}
}

// hostOptions describes the host startHost launches and the plugin it loads.
type hostOptions struct {
	// hostSource is the CLIProxyAPI checkout to build the server from.
	hostSource string
	// dir holds the whole layout: the plugin, the credentials, the config and
	// the log. The caller owns it so it can name paths the plugin build needs.
	dir string
	// pluginID is the plugin's id, which is also its library file name.
	pluginID string
	// pluginPackage is the package built as the shared library, and
	// pluginBuildDir the directory that build runs in.
	pluginPackage, pluginBuildDir string
	// pluginLDFlags, when set, is passed to the plugin build as -ldflags.
	pluginLDFlags string
	// credentialPrefix names the two fabricated Claude credentials' email
	// addresses and access tokens. No provider is ever reached with them.
	credentialPrefix string
	// hostSettings is YAML spliced in above the plugins block, and
	// pluginSettings YAML for the plugin's own config, indented six spaces.
	hostSettings, pluginSettings string
	// modelID is the model the host must be serving before startHost returns.
	modelID string
	// clientTimeout bounds every request made through the returned client.
	clientTimeout time.Duration
	// extraLogs are files dumped beside the server log when the test fails,
	// keyed by the label to print them under.
	extraLogs map[string]string
}

// liveHost is a running CLIProxyAPI with the plugin loaded and the model
// already served. It is torn down when the test ends.
type liveHost struct {
	// done closes when the server process exits, so a wait loop can fail fast
	// instead of polling a host that is already gone.
	done <-chan struct{}
	// upstreamURL is the refusing fixture proxy, reachable as an ordinary HTTP
	// target for a plugin that makes its own host.http.do call.
	upstreamURL   string
	baseURL       string
	client        *http.Client
	logPath       string
	apiKey        string
	managementKey string
}

// startHost builds the plugin and a CLIProxyAPI server, writes a config and
// two fabricated Claude credentials, launches the server against an upstream
// that refuses every connection, and returns once the host serves the model.
//
// Nothing here reaches a provider: the proxy answers every upstream dial with
// 502, and the credentials are fabricated.
func startHost(t *testing.T, opts hostOptions) *liveHost {
	t.Helper()

	host := &liveHost{
		apiKey:        opts.pluginID + "-api-key",
		managementKey: opts.pluginID + "-management-key",
	}

	// Every upstream dial lands here and is refused, so a request fails after
	// credential selection rather than reaching a provider.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fixture proxy refuses upstream", http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)

	pluginDir := filepath.Join(opts.dir, "plugins")
	authDir := filepath.Join(opts.dir, "auth")
	for _, path := range []string{pluginDir, authDir, filepath.Join(opts.dir, "home")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	extension, serverName := libraryExtension()

	// The plugin id is the library file name, so the build output is named
	// for it.
	args := []string{"build", "-buildmode=c-shared"}
	if opts.pluginLDFlags != "" {
		args = append(args, "-ldflags", opts.pluginLDFlags)
	}
	args = append(args, "-o", filepath.Join(pluginDir, opts.pluginID+extension), opts.pluginPackage)
	buildPlugin := exec.Command("go", args...)
	buildPlugin.Dir = opts.pluginBuildDir
	buildPlugin.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := buildPlugin.CombinedOutput(); err != nil {
		t.Fatalf("build plugin %s: %v\n%s", opts.pluginPackage, err, out)
	}

	serverPath := filepath.Join(opts.dir, serverName)
	buildServer := exec.Command("go", "build", "-o", serverPath, "./cmd/server")
	buildServer.Dir = opts.hostSource
	if out, err := buildServer.CombinedOutput(); err != nil {
		t.Fatalf("build CLIProxyAPI: %v\n%s", err, out)
	}

	for index, name := range []string{"claude-a.json", "claude-b.json"} {
		body := fmt.Sprintf(`{"type":"claude","email":"%s-%d@example.com","access_token":"%s-token-%d","refresh_token":"%s-refresh","expired":"2099-01-01T00:00:00Z"}`,
			opts.credentialPrefix, index, opts.credentialPrefix, index, opts.credentialPrefix)
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	port := unusedTCPPort(t)
	configPath := filepath.Join(opts.dir, "config.yaml")
	configYAML := fmt.Sprintf(`host: "127.0.0.1"
port: %d
proxy-url: %q
auth-dir: %q
api-keys: [%q]
remote-management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
  disable-auto-update-panel: true
logging-to-file: false
debug: false
disable-cooling: true
%splugins:
  enabled: true
  dir: %q
  configs:
    %s:
      enabled: true
      priority: 100
%s`, port, upstream.URL, authDir, host.apiKey, host.managementKey,
		opts.hostSettings, pluginDir, opts.pluginID, opts.pluginSettings)
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	host.logPath = filepath.Join(opts.dir, "server.log")
	logFile, err := os.Create(host.logPath)
	if err != nil {
		t.Fatal(err)
	}
	server := exec.Command(serverPath, "-config", configPath, "-local-model")
	server.Dir = opts.hostSource
	server.Stdout, server.Stderr = logFile, logFile
	server.Env = append(os.Environ(),
		"HOME="+filepath.Join(opts.dir, "home"),
		"HTTP_PROXY=", "HTTPS_PROXY=", "ALL_PROXY=",
		"NO_PROXY=127.0.0.1,localhost",
	)
	if err = server.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	go func() {
		_ = server.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		select {
		case <-processDone:
		default:
			_ = server.Process.Kill()
			<-processDone
		}
		_ = logFile.Close()
		if t.Failed() {
			t.Logf("server log:\n%s", readFile(host.logPath))
			for label, path := range opts.extraLogs {
				t.Logf("%s:\n%s", label, readFile(path))
			}
		}
	})

	host.done = processDone
	host.upstreamURL = upstream.URL
	host.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	host.client = &http.Client{Timeout: opts.clientTimeout}
	waitForModel(t, host.client, host.baseURL, host.apiKey, processDone, host.logPath, opts.modelID)
	return host
}

// post sends one Anthropic Messages request and discards the response, which
// the refusing upstream makes a 502 in every case.
func (h *liveHost) post(t *testing.T, body string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, h.baseURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.apiKey)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		t.Fatalf("post /v1/messages: %v\nserver log:\n%s", err, readFile(h.logPath))
	}
	_ = response.Body.Close()
}
