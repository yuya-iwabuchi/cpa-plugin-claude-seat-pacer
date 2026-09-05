// Package web serves the plugin's embedded status app.
//
// The host mounts these routes without authentication, so every view is
// read-only: the app issues no mutating request and model.Status carries ids
// and labels but no credential material. The whole app — markup, styles and
// script — is one embedded document with no external reference, so it renders
// on a host with no outbound network.
package web

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

//go:embed index.html
var indexHTML []byte

// contentSecurityPolicy confines the page to the inline style and script of
// this document. No directive names a remote origin, and connect-src 'self'
// is what lets the page reach its own api/status.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'unsafe-inline'; " +
	"style-src 'unsafe-inline'; " +
	"img-src data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// Source supplies the state the app renders.
type Source interface {
	// Status evaluates every credential for modelID at now. modelID "" lets
	// the source choose a default.
	Status(now time.Time, modelID string) model.Status
}

// NewHandler serves the app. The caller mounts it with the URL prefix already
// stripped, so it sees "/", "/index.html", and "/api/status".
func NewHandler(src Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html":
			if !allowRead(w, r) {
				return
			}
			servePage(w)
		case "/api/status":
			if !allowRead(w, r) {
				return
			}
			serveStatus(w, r, src)
		default:
			setCommonHeaders(w)
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

// allowRead rejects any method that could imply a write. The app is served
// unauthenticated, so nothing here accepts a request body.
func allowRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	setCommonHeaders(w)
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// setCommonHeaders applies the policy every response carries. Status is live
// operational state, so no response is cacheable.
func setCommonHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

func servePage(w http.ResponseWriter) {
	setCommonHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func serveStatus(w http.ResponseWriter, r *http.Request, src Source) {
	status := src.Status(time.Now(), r.URL.Query().Get("model"))

	// The body is built before any header is written so an encoding failure
	// can still produce a 500 rather than a truncated 200.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(status); err != nil {
		setCommonHeaders(w)
		http.Error(w, "status encode failed", http.StatusInternalServerError)
		return
	}

	setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
