package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/session"
)

// apiSocketName is the local API socket's file, beside the history file.
const apiSocketName = "api.sock"

// seatsPath is the one path the socket serves, the status app's seats route.
const seatsPath = "/api/seats"

// maxSocketPath is the longest socket path every supported platform binds:
// sun_path holds 104 bytes on macOS and 108 on Linux, its NUL included.
const maxSocketPath = 103

// localAPIStopTimeout bounds the wait for requests in flight when the socket
// closes. An answer makes no network call and no host callback, so one
// finishes in milliseconds.
const localAPIStopTimeout = 2 * time.Second

// localAPISupported reports whether this platform serves the local API socket.
// Windows does not, and a web.socket left on there is no fault to report.
var localAPISupported = goruntime.GOOS != "windows"

// localAPIServer is the running socket server. done closes once Serve returns.
// file is the socket file it bound, and stopped turns its log lines away once
// it is closing.
type localAPIServer struct {
	srv     *http.Server
	ln      *net.UnixListener
	file    os.FileInfo
	done    chan struct{}
	stopped atomic.Bool
}

// ownsPath reports whether path still holds the socket file the server bound.
func (gs *localAPIServer) ownsPath(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && os.SameFile(info, gs.file)
}

// SessionBinding returns the binding a Claude Code session's root agent holds
// for modelID, or, when it holds none for that model, its most recently used
// one. Claude Code sends side requests on a smaller model under the same
// session, and each model is bound on its own. It reads the table without
// refreshing any binding's idle timer. expiresAt is when the binding's idle
// time passes the affinity TTL, zero when the TTL never expires one. A binding
// for another model holds nothing for modelID's next request, so it reports
// as expired when it was last seen.
func (p *Plugin) SessionBinding(sessionID, modelID string) (b model.Binding, expiresAt time.Time, ok bool) {
	b, matched, ok := p.sessionBinding(sessionID, modelID)
	switch ttl := p.config().Affinity.TTL; {
	case !ok:
	case modelID != "" && !matched:
		expiresAt = b.LastSeen
	case ttl > 0:
		expiresAt = b.LastSeen.Add(ttl)
	}
	return b, expiresAt, ok
}

// sessionBinding finds the binding SessionBinding names, and whether it is the
// one for modelID.
func (p *Plugin) sessionBinding(sessionID, modelID string) (model.Binding, bool, bool) {
	keys := session.ClaudeCodeKeys(sessionID)
	if len(keys) == 0 {
		return model.Binding{}, false, false
	}
	var newest, forModel model.Binding
	var any, matched bool
	for _, b := range p.bindingStore().All() {
		if !slices.Contains(keys, b.SessionKey) {
			continue
		}
		if !any || b.LastSeen.After(newest.LastSeen) {
			newest, any = b, true
		}
		if modelID != "" && strings.EqualFold(b.Model, modelID) && (!matched || b.LastSeen.After(forModel.LastSeen)) {
			forModel, matched = b, true
		}
	}
	if matched {
		return forModel, true, true
	}
	return newest, false, any
}

// APISocketPath is where the local API socket is bound: the configured path,
// or beside the history file in the plugin's own data directory. Empty when
// neither is known.
func (p *Plugin) APISocketPath() string {
	if p.opts.APISocket != "" {
		return p.opts.APISocket
	}
	if p.opts.HistoryFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p.opts.HistoryFile), apiSocketName)
}

// syncLocalAPI starts the socket server while web.enabled and web.socket are
// both on and a data directory is known, and stops it otherwise. A server
// whose socket file is gone, or is no longer the file it bound, is started
// again. A start that fails records why, for the status page. Configure and
// every poll call it, so a failed start is tried again on the next poll.
func (p *Plugin) syncLocalAPI() {
	p.localAPIMu.Lock()
	defer p.localAPIMu.Unlock()
	cfg := p.config()
	path := p.APISocketPath()
	want := localAPISupported && cfg.Web.Enabled && cfg.Web.Socket && path != ""
	if want && p.localAPI != nil {
		if p.localAPI.ownsPath(path) {
			return
		}
		p.stopLocalAPILocked()
	}
	switch {
	case want:
		gs, err := p.startLocalAPI()
		p.mu.Lock()
		p.localAPIErr = ""
		if err != nil {
			p.localAPIErr = err.Error()
		}
		p.mu.Unlock()
		p.localAPI = gs
	default:
		p.stopLocalAPILocked()
		p.mu.Lock()
		p.localAPIErr = ""
		p.mu.Unlock()
	}
}

// stopLocalAPI closes the socket and waits for its server to return. It is
// idempotent.
func (p *Plugin) stopLocalAPI() {
	p.localAPIMu.Lock()
	defer p.localAPIMu.Unlock()
	p.stopLocalAPILocked()
}

// stopLocalAPILocked closes the running server. Closing removes the socket file
// only while it is still the one the server bound: another process may have
// bound the path since.
func (p *Plugin) stopLocalAPILocked() {
	gs := p.localAPI
	if gs == nil {
		return
	}
	p.localAPI = nil
	gs.stopped.Store(true)
	if !gs.ownsPath(p.APISocketPath()) {
		gs.ln.SetUnlinkOnClose(false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), localAPIStopTimeout)
	defer cancel()
	if gs.srv.Shutdown(ctx) != nil {
		_ = gs.srv.Close()
	}
	<-gs.done
}

// startLocalAPI binds the socket and serves the seats route on it. The socket
// is the host user's alone: its directory is the user's own and admits no one
// else, and the socket itself is 0600. Only the seats route is served, and
// the status app answers it with no host callback, so a request in flight
// never holds a host call open across an unload. A handler that panics is
// recovered by net/http, which reports it through the host's log until the
// server starts closing.
func (p *Plugin) startLocalAPI() (*localAPIServer, error) {
	ln, file, err := listenLocalAPI(p.APISocketPath())
	if err != nil {
		return nil, err
	}
	gs := &localAPIServer{ln: ln, file: file, done: make(chan struct{})}
	gs.srv = &http.Server{
		Handler:           http.HandlerFunc(p.serveLocalAPI),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          log.New(localAPILog{p, gs}, "", 0),
	}
	go func() {
		defer close(gs.done)
		p.guard("local API socket", func() { _ = gs.srv.Serve(ln) })
	}()
	return gs, nil
}

// localAPILog carries the socket server's own error lines to the host's log,
// and drops them once the server is closing: a handler can outlast the stop's
// wait, and the host may by then have unloaded the plugin.
type localAPILog struct {
	p  *Plugin
	gs *localAPIServer
}

func (l localAPILog) Write(b []byte) (int, error) {
	if l.gs.stopped.Load() {
		return len(b), nil
	}
	line := strings.TrimSpace(string(b))
	l.p.host.spawn(func() {
		l.p.host.log("warn", "claude-seat-pacer local API: "+line, nil)
	})
	return len(b), nil
}

// serveLocalAPI hands the seats route to the status app and refuses every
// other path, so the socket serves nothing the page's routes do not.
func (p *Plugin) serveLocalAPI(w http.ResponseWriter, r *http.Request) {
	h := p.resource.Load()
	if r.URL.Path != seatsPath || h == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.h.ServeHTTP(w, r)
}

// listenLocalAPI binds a Unix socket at path, in a directory it first makes the
// user's alone: one it creates is 0700, and an existing one must be a real
// directory the user owns, its group and other bits cleared. A socket file
// left by a run that ended without closing it is replaced; one a live process
// still answers on, or a file of another kind, is left alone and the bind
// fails. It returns the listener and the socket file it bound.
func listenLocalAPI(path string) (*net.UnixListener, os.FileInfo, error) {
	if len(path) > maxSocketPath {
		return nil, nil, fmt.Errorf("the local API socket path is %d bytes, past the %d a socket allows", len(path), maxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create the local API socket's directory: %w", err)
	}
	if err := privateDir(dir); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != fs.ModeSocket {
			return nil, nil, errors.New("the local API socket path holds a file that is not a socket")
		}
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			_ = conn.Close()
			return nil, nil, errors.New("another process is serving the local API socket")
		}
		if err := os.Remove(path); err != nil {
			return nil, nil, fmt.Errorf("remove a stale local API socket: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("inspect the local API socket path: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("bind the local API socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("restrict the local API socket: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = ln.Close()
		return nil, nil, fmt.Errorf("inspect the local API socket: %w", err)
	}
	return ln, info, nil
}
