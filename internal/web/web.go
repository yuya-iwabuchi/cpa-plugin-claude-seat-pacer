// Package web serves the plugin's embedded status app.
//
// The host mounts these routes without authentication, so everything they
// expose is read-only and reduced for an anonymous reader: the app issues no
// mutating request, model.Status carries ids and labels but no credential
// material, and serveStatus masks a label that is an account email, keeps only
// the config block the page reads, strips URLs out of the operator warnings and
// bounds the binding list. The authenticated management route serves the same
// status unreduced.
//
// A credential's id survives the reduction whole: every table on the page
// correlates on it, and masking is not injective, so two seats sharing a
// domain would merge.
//
// The whole app — markup, styles and script — is one embedded document with no
// external reference, so it renders on a host with no outbound network.
package web

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

//go:embed index.html
var indexHTML []byte

// maxStatusBindings caps the binding rows one status response carries. The
// store holds up to affinity.max-sessions entries, far more than a page reads,
// and this route re-encodes the list on every poll.
const maxStatusBindings = 500

// contentSecurityPolicy confines script execution to the one inline block of
// this document, named by hash. No directive names a remote origin, and
// connect-src 'self' is what lets the page reach its own api/status. Inline
// style attributes in the markup keep style-src on 'unsafe-inline', which a
// hash cannot cover.
var contentSecurityPolicy = "default-src 'none'; " +
	"script-src " + inlineScriptSource(indexHTML) + "; " +
	"style-src 'unsafe-inline'; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// inlineScriptSource is the script-src expression covering the document's
// inline block: its SHA-256 hash, or 'unsafe-inline' for a document whose
// script block cannot be located, so the page still runs. TestHeaders holds
// the shipped policy to the hash.
//
// The hash covers the file's bytes, which is the script a browser hashes only
// while the document carries neither NUL nor CR: the HTML tokenizer rewrites
// those inside script data to U+FFFD and LF, and a policy naming any other
// text blocks the document whole. TestPageHoldsNoRewrittenByte keeps them out.
func inlineScriptSource(page []byte) string {
	const openTag, closeTag = "<script>", "</script>"
	i := bytes.Index(page, []byte(openTag))
	j := bytes.Index(page, []byte(closeTag))
	if i < 0 || j < i+len(openTag) {
		return "'unsafe-inline'"
	}
	sum := sha256.Sum256(page[i+len(openTag) : j])
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// Source supplies the state the app renders.
type Source interface {
	// Status evaluates every credential for modelID at now. modelID "" lets
	// the source choose a default.
	Status(now time.Time, modelID string) model.Status
}

// NewHandler serves the app. The caller mounts it with the URL prefix already
// stripped, so it sees "/index.html" and "/api/status". A bare "/" reaches it
// only from cmd/webdev: the host matches a resource route by exact path and
// refuses an empty one (internal/runtime/management.go).
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
	status := reduceForPublic(src.Status(time.Now(), r.URL.Query().Get("model")))

	// The body is built before any header is written so an encoding failure
	// can still produce a 500 rather than a truncated 200.
	body, err := encodeStatus(status)
	if err != nil {
		setCommonHeaders(w)
		http.Error(w, "status encode failed", http.StatusInternalServerError)
		return
	}

	setCommonHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// nonFiniteSentinel stands in for a float64 that JSON has no literal for. A
// utilization reading never reaches it, and the encoder renders it as one
// exact literal, which encodeStatus rewrites to null so the page reads the
// value as absent rather than as zero.
const nonFiniteSentinel = -math.MaxFloat64

// nonFiniteLiteral is how encoding/json renders nonFiniteSentinel.
// TestNonFiniteSurvivesEncoding holds the two together.
var nonFiniteLiteral = []byte("-1.7976931348623157e+308")

var jsonNull = []byte("null")

func encodeStatus(status model.Status) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(status); err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(buf.Bytes(), nonFiniteLiteral, jsonNull), nil
}

// reduceForPublic is the status as the unauthenticated route serves it. It
// copies every slice it rewrites, so the source's own state is untouched.
func reduceForPublic(st model.Status) model.Status {
	// The page reads the pace curve out of the config and nothing else. The
	// rest is operator configuration, the usage endpoint above all: it is
	// operator-set and may name internal infrastructure.
	st.Config = model.Config{Pace: finitePace(st.Config.Pace)}

	warnings := make([]string, 0, len(st.Warnings)+1)
	for _, w := range st.Warnings {
		warnings = append(warnings, publicWarning(w))
	}
	st.Warnings = warnings

	auths := make([]model.AuthStatus, len(st.Auths))
	for i, a := range st.Auths {
		a.Label = publicLabel(a.Label)
		a.Snapshot.Label = publicLabel(a.Snapshot.Label)
		a.Snapshot.Windows = finiteWindows(a.Snapshot.Windows)
		a.Score = finiteScore(a.Score)
		auths[i] = a
	}
	st.Auths = auths

	decisions := make([]model.Decision, len(st.Decisions))
	for i, d := range st.Decisions {
		if len(d.Scores) > 0 {
			scores := make([]model.Score, len(d.Scores))
			for j, s := range d.Scores {
				scores[j] = finiteScore(s)
			}
			d.Scores = scores
		}
		decisions[i] = d
	}
	st.Decisions = decisions

	if total := len(st.Bindings); total > maxStatusBindings {
		st.Bindings = st.Bindings[:maxStatusBindings]
		st.Warnings = append(st.Warnings, fmt.Sprintf(
			"binding list truncated to %d of %d entries for this view",
			maxStatusBindings, total))
	}
	return st
}

// absoluteURL matches a scheme-qualified URL, which ends at the first space or
// quote: a warning quotes one inside a Go transport error.
var absoluteURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"']*`)

// publicWarning is an operator warning with its URLs taken out. A failing poll
// quotes the transport error whole, and that error names the usage endpoint
// this route otherwise withholds.
func publicWarning(w string) string {
	return absoluteURL.ReplaceAllString(w, "…")
}

// publicLabel masks a label that is an account email. The host label falls
// back to the credential's email address, which this route would otherwise
// hand to anyone who can reach the port; the domain still tells the operator
// which organization a seat belongs to.
func publicLabel(label string) string {
	at := strings.LastIndex(label, "@")
	if at <= 0 || at == len(label)-1 {
		return label
	}
	local, domain := label[:at], label[at+1:]
	if !strings.Contains(domain, ".") || strings.ContainsAny(domain, " \t") {
		return label
	}
	first := []rune(local)[0]
	return string(first) + "…@" + domain
}

// finite replaces a value JSON cannot express with the sentinel encodeStatus
// turns into null.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nonFiniteSentinel
	}
	return f
}

func finiteWindows(windows []model.Window) []model.Window {
	if len(windows) == 0 {
		return windows
	}
	out := make([]model.Window, len(windows))
	for i, w := range windows {
		w.Utilization = finite(w.Utilization)
		out[i] = w
	}
	return out
}

func finiteScore(s model.Score) model.Score {
	s.Total = finite(s.Total)
	s.RawPenalty = finite(s.RawPenalty)
	if len(s.Windows) == 0 {
		return s
	}
	out := make([]model.WindowScore, len(s.Windows))
	for i, ws := range s.Windows {
		ws.Elapsed = finite(ws.Elapsed)
		ws.Target = finite(ws.Target)
		ws.Utilization = finite(ws.Utilization)
		ws.Slack = finite(ws.Slack)
		ws.Weight = finite(ws.Weight)
		out[i] = ws
	}
	s.Windows = out
	return s
}

func finitePace(p model.PaceConfig) model.PaceConfig {
	p.CurveExponent = finite(p.CurveExponent)
	p.LandingTarget = finite(p.LandingTarget)
	p.WeeklyWeight = finite(p.WeeklyWeight)
	p.SessionWeight = finite(p.SessionWeight)
	p.ScopedWeight = finite(p.ScopedWeight)
	p.RawWeight = finite(p.RawWeight)
	p.HysteresisMargin = finite(p.HysteresisMargin)
	p.HardCutoff = finite(p.HardCutoff)
	return p
}
