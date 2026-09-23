// Package web serves the plugin's embedded status app.
//
// The page itself is a resource route, which the host serves to anyone who can
// reach its port; it is static and carries no data. The status it renders comes
// from the plugin's page-status management route, which the host answers only
// with the management key and, by default, only for loopback clients. The app
// issues no mutating request, model.Status carries ids and labels but no
// credential material, and serveStatus still names each seat without its
// account address, withholds the credential file name, keeps only the config
// block the page reads, strips URLs, filesystem paths and network addresses out
// of the operator warnings and bounds the binding list, so the page is safe to
// show on a screen or in a screenshot. The management status route serves the
// same status unreduced.
//
// A credential's id is published as a truncated HMAC of the real one rather
// than masked: every table on the page correlates on the id, and masking is
// not injective, so two seats sharing a domain would merge into one row. The
// HMAC is keyed with a secret of the install's own, because the real id is
// routinely derived from an account address, and an unkeyed digest of it lets
// anyone holding a screenshot confirm a guessed address. Seat names are made
// unique the same way, with a tag of that HMAC, because two organizations
// registered under one address share every other name.
//
// The whole app — markup, styles and script — is one embedded document with no
// external reference, so it renders on a host with no outbound network.
package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
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
// hash cannot cover. The Management Center reaches this page by embedding it
// in an iframe of the same origin, so frame-ancestors admits 'self' and no
// one else.
var contentSecurityPolicy = "default-src 'none'; " +
	"script-src " + inlineScriptSource(indexHTML) + "; " +
	"style-src 'unsafe-inline'; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'self'"

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

// Syncer is a Source that can re-read upstream usage on demand, throttled by
// the source itself. A Source that does not implement it serves whatever its
// own polling has most recently established.
type Syncer interface {
	// SyncNow re-reads usage unless it was read too recently, and reports
	// whether it read.
	SyncNow(ctx context.Context) bool
}

// Source supplies the state the app renders.
type Source interface {
	// Status evaluates every credential for modelID at now. modelID "" lets
	// the source choose a default.
	Status(now time.Time, modelID string) model.Status
}

// NewHandler serves the app at app-relative paths: "/index.html" for the page
// and "/api/status" for its data, which the plugin reaches from the page-status
// management route. A bare "/" reaches it only from cmd/webdev: the host
// matches a resource route by exact path and refuses an empty one
// (internal/runtime/management.go).
func NewHandler(src Source) http.Handler {
	return newHandler(src, newIDKey(src).get)
}

// newHandler is NewHandler with the key published ids are derived with
// supplied by the caller.
func newHandler(src Source, key func() []byte) http.Handler {
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
			serveStatus(w, r, src, key())
		default:
			setCommonHeaders(w)
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

// allowRead rejects any method that could imply a write. The page is served
// with no key, so nothing here accepts a request body.
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

func serveStatus(w http.ResponseWriter, r *http.Request, src Source, key []byte) {
	// A sync is a read of the provider rather than a write of any local state,
	// which is why a read-only route may serve it at all. The source throttles
	// it; this route only asks.
	if r.URL.Query().Get("sync") == "1" {
		if s, ok := src.(Syncer); ok {
			s.SyncNow(r.Context())
		}
	}
	status := reduceForPage(src.Status(time.Now(), r.URL.Query().Get("model")), key)

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
	if err := enc.Encode(status); err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(buf.Bytes(), nonFiniteLiteral, jsonNull), nil
}

// reduceForPage is the status as the page's route serves it, with every
// credential id published under key. It copies every slice it rewrites, so the
// source's own state is untouched.
func reduceForPage(st model.Status, key []byte) model.Status {
	// The page reads the pace curve and the poll cadence out of the config and
	// nothing else. The rest is operator configuration, the usage endpoint
	// above all: it is operator-set and may name internal infrastructure. A
	// cadence names nothing, and the page states it beside the countdown.
	st.Config = model.Config{
		Enabled: st.Config.Enabled,
		Pace:    finitePace(st.Config.Pace),
		Quota:   model.QuotaConfig{PollInterval: st.Config.Quota.PollInterval},
	}

	publish := func(authID string) string { return publicID(key, authID) }
	ids := authIDReplacer(st, publish)

	// The cap applies before the rewrite, so a store running to
	// affinity.max-sessions costs only the rows the response carries.
	truncated := 0
	if total := len(st.Bindings); total > maxStatusBindings {
		truncated = total
		st.Bindings = st.Bindings[:maxStatusBindings]
	}

	warnings := make([]string, 0, len(st.Warnings)+1)
	for _, w := range st.Warnings {
		warnings = append(warnings, publicText(w, ids))
	}
	st.Warnings = warnings

	names := publicSeatLabels(st.Auths, publish)
	auths := make([]model.AuthStatus, len(st.Auths))
	for i, a := range st.Auths {
		a.Label = names[i]
		a.Email = maskEmail(seatEmail(a))
		// The file name carries the account's local part more often than not,
		// and everything it distinguishes is already in Label.
		a.Name = ""
		a.AuthID = publish(a.AuthID)
		a.Snapshot.AuthID = publish(a.Snapshot.AuthID)
		// The host's runtime credential index is a digest of the credential's
		// file path, and no view on the page reads it.
		a.Snapshot.AuthIndex = ""
		a.Snapshot.Label = names[i]
		a.Snapshot.Err = publicText(a.Snapshot.Err, ids)
		a.Snapshot.Windows = finiteWindows(a.Snapshot.Windows)
		a.History = finiteHistory(a.History)
		a.Score = finiteScore(a.Score)
		a.Score.AuthID = publish(a.Score.AuthID)
		auths[i] = a
	}
	st.Auths = auths

	decisions := make([]model.Decision, len(st.Decisions))
	for i, d := range st.Decisions {
		d.ChosenAuthID = publish(d.ChosenAuthID)
		d.PreviousAuthID = publish(d.PreviousAuthID)
		d.Note = publicText(d.Note, ids)
		if len(d.Scores) > 0 {
			scores := make([]model.Score, len(d.Scores))
			for j, s := range d.Scores {
				s = finiteScore(s)
				s.AuthID = publish(s.AuthID)
				scores[j] = s
			}
			d.Scores = scores
		}
		decisions[i] = d
	}
	st.Decisions = decisions

	bindings := make([]model.Binding, len(st.Bindings))
	for i, b := range st.Bindings {
		b.AuthID = publish(b.AuthID)
		bindings[i] = b
	}
	st.Bindings = bindings

	if truncated > 0 {
		st.Warnings = append(st.Warnings, fmt.Sprintf(
			"binding list truncated to %d of %d entries for this view",
			maxStatusBindings, truncated))
	}
	return st
}

// publicIDBytes is how much of an HMAC-SHA256 a published credential id
// carries: 8 bytes, so 16 hex characters. Across the hundred credentials a
// pool holds at the outside, the birthday bound on 64 bits stays under 1e-15,
// so the id is injective in practice and the joins between the page's tables
// hold.
const publicIDBytes = 8

// publicID is the credential id this route publishes, keyed with key.
// CLIProxyAPI names a Claude OAuth credential file after the account and falls
// back to that name for the id, so the real id is routinely an email address.
// An empty id stays empty, which is how a decision records having no previous
// credential.
func publicID(key []byte, authID string) string {
	if authID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(authID))
	return hex.EncodeToString(mac.Sum(nil)[:publicIDBytes])
}

// authIDReplacer substitutes the published id for every credential id the
// status carries, for the free text that names a credential rather than
// carrying it in a field: an operator warning and a decision note both quote
// the id whole. publish is the published form of an id.
//
// Longest first, so an id that is a suffix of another does not consume it, and
// a Replacer never rescans what it has written.
func authIDReplacer(st model.Status, publish func(string) string) *strings.Replacer {
	seen := make(map[string]struct{})
	ids := make([]string, 0, len(st.Auths))
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, a := range st.Auths {
		add(a.AuthID)
		add(a.Snapshot.AuthID)
		add(a.Score.AuthID)
	}
	for _, b := range st.Bindings {
		add(b.AuthID)
	}
	for _, d := range st.Decisions {
		add(d.ChosenAuthID)
		add(d.PreviousAuthID)
		for _, s := range d.Scores {
			add(s.AuthID)
		}
	}
	slices.SortFunc(ids, func(a, b string) int {
		if n := len(b) - len(a); n != 0 {
			return n
		}
		return strings.Compare(a, b)
	})
	pairs := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		pairs = append(pairs, id, publish(id))
	}
	return strings.NewReplacer(pairs...)
}

// absoluteURL matches a scheme-qualified URL, which ends at the first space or
// quote: a transport error quotes one inside its message.
var absoluteURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"']*`)

// absolutePath is a filesystem path an os error names, which would tell anyone
// looking at the page where the host keeps its credentials.
var absolutePath = regexp.MustCompile(`(?:^|[\s"'(:])(?:/[^\s"'():]+){2,}`)

// windowsPath is a Windows filesystem path, drive-qualified or UNC (the \\?\
// long-path form among them), which an os error on that platform names. A
// segment may hold a space, as a user profile directory often does, so the
// path runs to the delimiter an os error closes it with rather than to the
// first space.
var windowsPath = regexp.MustCompile(`(?:\b[A-Za-z]:[\\/]|\\\\[^\s"'\\]+\\(?:[A-Za-z]:\\)?)(?:[^"'():;,\n]*[^\s"'():;,])?`)

// homePath is a path relative to a home directory, ~/ or ~user/.
var homePath = regexp.MustCompile(`(?:^|[\s"'(:=])~[A-Za-z0-9._-]*/[^\s"'():]*`)

// networkAddress is a host a Go transport error names without a scheme: an
// IPv4 or bracketed IPv6 address with or without its port, and a host name
// carrying a port, as a dial error quotes them.
var networkAddress = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b` +
	`|\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*(?:%[0-9A-Za-z._-]+)?\](?::\d{1,5})?` +
	`|\b(?:localhost|[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+):\d{1,5}\b`)

// lookupHost is the host a failed name resolution names, which carries no
// port: "lookup usage.corp.internal on 192.168.1.1:53" or "lookup
// usage.corp.internal: no such host".
var lookupHost = regexp.MustCompile(`\blookup [^\s:]+`)

// certificateNames is the name list a certificate mismatch quotes, which runs
// to the end of the error.
var certificateNames = regexp.MustCompile(`\b(certificate is (?:valid for|not valid for any names, but wanted to match)) [^:;"]+`)

// bareEmail matches an account address inside free text. A credential this
// status no longer holds a row for is still named by a decision note that
// outlives it, and that name is an email address.
var bareEmail = regexp.MustCompile(`[^\s"'<>@]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)

// publicText is operator-facing free text with everything it quotes that this
// route otherwise withholds taken out: URLs and network addresses, because a
// failing poll quotes the transport error and that error names the usage
// endpoint or the resolver, filesystem paths, and credential identity, because
// a warning and a decision note both name the credential they concern.
func publicText(s string, ids *strings.Replacer) string {
	if s == "" {
		return ""
	}
	s = absoluteURL.ReplaceAllString(s, "…")
	s = windowsPath.ReplaceAllString(s, "…")
	for _, re := range []*regexp.Regexp{absolutePath, homePath} {
		s = re.ReplaceAllStringFunc(s, keepDelimiter)
	}
	s = certificateNames.ReplaceAllString(s, "$1 …")
	s = lookupHost.ReplaceAllString(s, "lookup …")
	s = networkAddress.ReplaceAllString(s, "…")
	s = ids.Replace(s)
	return bareEmail.ReplaceAllStringFunc(s, maskEmail)
}

// keepDelimiter is the placeholder for a path match that may have opened on
// the delimiter before the path: the delimiter stays and the path goes.
func keepDelimiter(m string) string {
	if m[0] == '/' || m[0] == '~' {
		return "…"
	}
	return m[:1] + "…"
}

// seatEmail is the account address a credential row carries: the host's email
// field, else a label the host filled in from that same address.
func seatEmail(a model.AuthStatus) string {
	if a.Email != "" {
		return a.Email
	}
	if bareEmail.MatchString(a.Label) && maskEmail(a.Label) != a.Label {
		return a.Label
	}
	return ""
}

// publicSeatLabels is the Label of every seat, in row order, as the page's
// route serves it. Every name is unique across the
// rows, so the two credentials one account holds in two organizations stay
// apart on the page.
//
// A name comes from the first of these that yields one: a label the operator
// set on the host, the credential's file name with the account address taken
// out of it, and the masked address. Two rows that still share a name each
// carry a tag of their published id, publish(AuthID), which is stable across
// restarts while the key it is derived with persists.
func publicSeatLabels(auths []model.AuthStatus, publish func(string) string) []string {
	names := make([]string, len(auths))
	count := make(map[string]int, len(auths))
	for i, a := range auths {
		email := seatEmail(a)
		residue := fileNameResidue(a.Name, email, a.Provider)
		switch {
		case a.Label != "" && a.Label != email:
			names[i] = maskEmail(a.Label)
		case residue != "":
			names[i] = residue
		default:
			names[i] = maskEmail(email)
		}
		count[names[i]]++
	}
	for i, a := range auths {
		if count[names[i]] < 2 {
			continue
		}
		// An empty id hashes to nothing; the row index stands in for it.
		tag := "#" + publish(a.AuthID)
		if len(tag) < 1+seatTagLen {
			tag = "#" + strconv.Itoa(i)
		} else {
			tag = tag[:1+seatTagLen]
		}
		if names[i] == "" {
			names[i] = tag
		} else {
			names[i] += " " + tag
		}
	}
	return names
}

// seatTagLen is how many hex characters of the published id a seat tag
// carries. Four is short enough to read aloud and, with sixteen bits, keeps a
// pool of a dozen same-named seats clear of collisions in practice.
const seatTagLen = 4

// nameSeparators are the characters a credential file name uses between its
// parts, which a removed part leaves dangling.
const nameSeparators = "-_. "

// fileNameResidue is what an operator wrote into a credential file name over
// and above what the host derives: the extension, the provider, and the
// account address or its local part are taken out, and the separators they
// leave behind are trimmed. The host's default name is the address alone, so
// its residue is empty.
func fileNameResidue(name, email, provider string) string {
	if name == "" {
		return ""
	}
	name = strings.TrimSuffix(name, ".json")
	name = bareEmail.ReplaceAllString(name, "")
	if email != "" {
		name = deleteFold(name, email)
		if at := strings.Index(email, "@"); at > 0 {
			name = deleteFold(name, email[:at])
		}
	}
	if provider != "" {
		trimmed := strings.TrimLeft(name, nameSeparators)
		if len(trimmed) > len(provider) && strings.EqualFold(trimmed[:len(provider)], provider) &&
			strings.ContainsRune(nameSeparators, rune(trimmed[len(provider)])) {
			name = trimmed[len(provider):]
		}
	}
	for {
		next := strings.Trim(name, nameSeparators)
		next = strings.ReplaceAll(next, "--", "-")
		if next == name {
			return name
		}
		name = next
	}
}

// deleteFold removes every case-insensitive occurrence of old from s. It
// matches on s itself, because a lowercased copy is not byte-aligned with it:
// a letter such as U+023A or the Kelvin sign changes length when lowercased.
func deleteFold(s, old string) string {
	if old == "" {
		return s
	}
	return regexp.MustCompile("(?i)"+regexp.QuoteMeta(old)).ReplaceAllLiteralString(s, "")
}

// maskEmail masks an account email down to its first letter and domain. The
// host label falls back to the credential's email address, which this route
// would otherwise hand to anyone who can reach the port; the domain still
// tells the operator which organization a seat belongs to. A string that is
// not an email comes back unchanged.
func maskEmail(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return addr
	}
	local, domain := addr[:at], addr[at+1:]
	if !strings.Contains(domain, ".") || strings.ContainsAny(domain, " \t") {
		return addr
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
		return []model.Window{}
	}
	out := make([]model.Window, len(windows))
	for i, w := range windows {
		w.Utilization = finite(w.Utilization)
		out[i] = w
	}
	return out
}

// finiteHistory replaces every non-finite sample utilization with the
// sentinel. Sample.MarshalJSON refuses a non-finite value outright, so one
// such sample would otherwise fail the whole status encode.
func finiteHistory(hs []model.WindowHistory) []model.WindowHistory {
	if len(hs) == 0 {
		return hs
	}
	out := make([]model.WindowHistory, len(hs))
	for i, h := range hs {
		cycles := make([]model.Cycle, len(h.Cycles))
		for j, c := range h.Cycles {
			samples := make([]model.Sample, len(c.Samples))
			for k, s := range c.Samples {
				s.Utilization = finite(s.Utilization)
				samples[k] = s
			}
			c.Samples = samples
			cycles[j] = c
		}
		h.Cycles = cycles
		out[i] = h
	}
	return out
}

func finiteScore(s model.Score) model.Score {
	s.Cost = finite(s.Cost)
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
	p.Steepness = finite(p.Steepness)
	p.LandingTarget = finite(p.LandingTarget)
	p.WeeklyWeight = finite(p.WeeklyWeight)
	p.SessionWeight = finite(p.SessionWeight)
	p.ScopedWeight = finite(p.ScopedWeight)
	p.HysteresisMargin = finite(p.HysteresisMargin)
	return p
}
