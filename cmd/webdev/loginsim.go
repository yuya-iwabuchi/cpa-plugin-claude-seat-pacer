package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// loginSession is one sign-in the simulated host has started: pending until
// the fake Claude page answers it, then done or failed.
type loginSession struct {
	status string
	err    string
}

// loginSim stands in for the host's Claude OAuth routes the status page calls
// under api/, and for Claude's sign-in page, which offers every team the
// pool's seats belong to plus one no seat holds. Choosing a team logs in to it
// the way the host does: the seat with that team gets a fresh login, and a
// team no seat holds adds a new seat.
type loginSim struct {
	f        *fixture
	sessions map[string]*loginSession
}

func newLoginSim(f *fixture) *loginSim {
	return &loginSim{f: f, sessions: map[string]*loginSession{}}
}

func (s *loginSim) wrap(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/anthropic-auth-url", s.authURL)
	mux.HandleFunc("/api/get-auth-status", s.status)
	mux.HandleFunc("/api/oauth-session", s.cancel)
	mux.HandleFunc("/api/oauth-callback", s.callback)
	mux.HandleFunc("/fake-claude/authorize", s.authorize)
	mux.HandleFunc("/fake-claude/choose", s.choose)
	mux.Handle("/", next)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *loginSim) authURL(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	s.f.mu.Lock()
	s.sessions[state] = &loginSession{status: "wait"}
	s.f.mu.Unlock()
	writeJSON(w, map[string]string{"status": "ok", "state": state, "url": "/fake-claude/authorize?state=" + state})
}

func (s *loginSim) status(w http.ResponseWriter, r *http.Request) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	sess, ok := s.sessions[r.URL.Query().Get("state")]
	switch {
	case !ok:
		writeJSON(w, map[string]string{"status": "error", "error": "unknown or expired state"})
	case sess.status == "error":
		writeJSON(w, map[string]string{"status": "error", "error": sess.err})
	default:
		writeJSON(w, map[string]string{"status": sess.status})
	}
}

func (s *loginSim) cancel(w http.ResponseWriter, r *http.Request) {
	s.f.mu.Lock()
	delete(s.sessions, r.URL.Query().Get("state"))
	s.f.mu.Unlock()
	writeJSON(w, map[string]string{"status": "ok"})
}

// callback takes the address a remote browser landed on, which carries the
// state, and the team chosen when the address came from this simulation; a
// pasted address without one logs in to the first seat's team.
func (s *loginSim) callback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider    string `json:"provider"`
		RedirectURL string `json:"redirect_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	u, err := url.Parse(body.RedirectURL)
	if err != nil || u.Query().Get("state") == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"status": "error", "error": "invalid redirect_url"})
		return
	}
	team := u.Query().Get("team")
	if team == "" && u.Query().Get("error") == "" {
		s.f.mu.Lock()
		team = s.teams()[0]
		s.f.mu.Unlock()
	}
	s.finish(u.Query().Get("state"), team, u.Query().Get("error"))
	writeJSON(w, map[string]string{"status": "ok"})
}

// teams is every seat's team and one no seat holds; the caller holds f.mu.
func (s *loginSim) teams() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range s.f.auths {
		if a.Login != nil && a.Login.Team != "" && !seen[a.Login.Team] {
			seen[a.Login.Team] = true
			out = append(out, a.Login.Team)
		}
	}
	return append(out, "Acme Research")
}

const fakeClaudeCSS = `body{font:15px system-ui;background:#1f1e1b;color:#eee;display:grid;place-items:center;min-height:100vh;margin:0}
.box{background:#2a2925;padding:28px 32px;border-radius:12px;width:420px}h1{font-size:18px;margin:0 0 6px}p{color:#aaa;margin:0 0 16px}
a.org{display:block;padding:10px 14px;margin:6px 0;border:1px solid #444;border-radius:8px;color:#eee;text-decoration:none}a.org:hover{background:#333}
a.deny{color:#aaa;font-size:13px}code{font-size:12px;word-break:break-all;color:#ccc}`

func (s *loginSim) authorize(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	s.f.mu.Lock()
	teams := s.teams()
	s.f.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "<!doctype html><meta charset=utf-8><title>Claude · authorize</title><style>%s</style><div class=box>", fakeClaudeCSS)
	b.WriteString("<h1>Authorize Claude Code</h1><p>Simulated sign-in. Choose an organization.</p>")
	for _, t := range teams {
		fmt.Fprintf(&b, `<a class=org href="/fake-claude/choose?state=%s&team=%s">%s</a>`, url.QueryEscape(state), url.QueryEscape(t), html.EscapeString(t))
	}
	fmt.Fprintf(&b, `<p style="margin-top:14px"><a class=deny href="/fake-claude/choose?state=%s&error=access_denied">Deny</a></p></div>`, url.QueryEscape(state))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (s *loginSim) choose(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.finish(q.Get("state"), q.Get("team"), q.Get("error"))
	landed := fmt.Sprintf("http://localhost:54545/callback?code=sim&state=%s&team=%s", url.QueryEscape(q.Get("state")), url.QueryEscape(q.Get("team")))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Signed in</title><style>%s</style><div class=box><h1>%s</h1><p>You can close this tab.</p><p>On a browser that is not on the host, this tab lands on an address that fails to load:</p><code>%s</code></div>",
		fakeClaudeCSS, map[bool]string{true: "Sign-in denied", false: "Signed in"}[q.Get("error") != ""], html.EscapeString(landed))
}

// finish completes a session and, for a team, applies the login to the pool.
func (s *loginSim) finish(state, team, errText string) {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	sess, ok := s.sessions[state]
	if !ok || sess.status != "wait" {
		return
	}
	if errText != "" || team == "" {
		sess.status, sess.err = "error", "Bad request"
		return
	}
	sess.status = "ok"
	now := time.Now().Add(s.f.shift)
	fresh := &model.Login{State: model.LoginOK, Team: team,
		AccessExpiresAt: ptr(now.Add(8 * time.Hour)), LoggedInAt: ptr(now), EndsAround: ptr(now.Add(model.LoginLifetime))}
	for i := range s.f.auths {
		a := &s.f.auths[i]
		if a.Login == nil || a.Login.Team != team {
			continue
		}
		a.Login, a.HostStatus = fresh, "active"
		a.Label = strings.SplitN(a.Label, " · ", 2)[0] + " · logged in again"
		snap := s.f.snapshots[a.AuthID]
		snap.Err, snap.ErrCategory = "", ""
		s.f.snapshots[a.AuthID] = snap
		s.f.observedAge[a.AuthID] = 5 * time.Second
		s.f.rebuildWarnings()
		return
	}
	n := len(s.f.auths) + 1
	ms := manySeat{id: fmt.Sprintf("claude-eng-%d.json", n), label: fmt.Sprintf("eng-%d · new seat", n), name: fmt.Sprintf("claude-eng-%d.json", n),
		session: 0, weekly: 0, scoped: 0, elapsedDays: 0.1}
	s.f.addManySeat(len(s.f.auths), ms)
	s.f.auths[len(s.f.auths)-1].Login = fresh
	s.f.auths[len(s.f.auths)-1].Bindings = 0
	s.f.rebuildWarnings()
}
