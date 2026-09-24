package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// stubSource records what the handler asked for and returns a canned status.
type stubSource struct {
	status  model.Status
	gotNow  time.Time
	gotModl string
	calls   int
}

func (s *stubSource) Status(now time.Time, modelID string) model.Status {
	s.calls++
	s.gotNow = now
	s.gotModl = modelID
	return s.status
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// testIDKey is the key testHandler publishes credential ids under, so a test
// can name the id a response carries.
var testIDKey = bytes.Repeat([]byte{0x5a}, idKeyBytes)

// testHandler is the handler NewHandler builds, publishing ids under testIDKey.
func testHandler(src Source) http.Handler {
	return newHandler(src, func() []byte { return testIDKey })
}

// testID is id as testHandler publishes it.
func testID(id string) string { return publicID(testIDKey, id) }

// base is the instant every fixture status is stamped with, so two fixtures
// never differ by a clock the assertions do not name.
var base = time.Date(2026, 9, 4, 15, 4, 5, 0, time.UTC)

// richStatus exercises every field the app reads, including the ones that only
// appear on an unhappy path.
func richStatus() model.Status {
	cfg := model.Defaults()
	cfg.Pace.CurveExponent = 1.35
	return model.Status{
		Now: base,
		Plugin: model.PluginInfo{
			Name:              "claude-seat-pacer",
			Version:           "0.4.2",
			HostSchemaVersion: 1,
			StartedAt:         base.Add(-3 * time.Hour),
		},
		Config: cfg,
		Model:  "claude-fable-5",
		Auths: []model.AuthStatus{{
			AuthID: "auth-a", Label: "Seat A", Provider: "claude", Priority: 10,
			HostStatus: "active", Bindings: 3,
			Snapshot: model.AuthSnapshot{
				AuthID: "auth-a", AuthIndex: "0", Label: "Seat A",
				ObservedAt: base.Add(-time.Minute), Source: model.SourceUsageEndpoint,
				Err: "boom", ErrCategory: "timeout",
				Windows: []model.Window{{
					Kind: model.WindowSession, Utilization: 0.8,
					ResetsAt: base.Add(2 * time.Hour), Duration: model.SessionDuration,
					Status: model.StatusAllowed, Severity: model.SeverityNormal, Active: true,
				}, {
					Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable, Utilization: 0.42,
					ResetsAt: base.Add(72 * time.Hour), Duration: model.WeeklyDuration,
				}},
			},
			Score: model.Score{
				AuthID: "auth-a", Cost: -0.31, Eligible: true,
				Windows: []model.WindowScore{{
					Kind: model.WindowSession, Elapsed: 0.6, Target: 0.49,
					Utilization: 0.8, Slack: -0.31, Weight: 0.35,
					ResetsAt: base.Add(2 * time.Hour),
				}},
			},
			Cache: model.CacheStats{
				Requests: 1412, CacheReadTokens: 9600000,
				CacheCreationTokens: 250000, FreshInputTokens: 150000, OutputTokens: 318400,
			},
			History: []model.WindowHistory{{
				Kind: model.WindowSession,
				Cycles: []model.Cycle{{
					ResetsAt: base.Add(2 * time.Hour),
					Samples:  []model.Sample{{At: base.Add(-time.Hour), Utilization: 0.5}, {At: base.Add(-time.Minute), Utilization: 0.8}},
				}},
				Locks: []model.Lock{
					{From: base.Add(-50 * time.Minute).Unix(), To: base.Add(-40 * time.Minute).Unix(), End: model.LockEndServed},
					{From: base.Add(-30 * time.Minute).Unix(), To: base.Add(-25 * time.Minute).Unix(), End: model.LockEndCleared},
					{From: base.Add(-5 * time.Minute).Unix(), To: base.Add(2 * time.Hour).Unix()},
				},
			}},
		}},
		Bindings: []model.Binding{{
			SessionKey: "5f2c0b7d", Provider: "claude", Model: "claude-fable-5",
			AuthID: "auth-a", BoundAt: base.Add(-time.Hour), LastSeen: base, Hits: 214,
		}},
		Decisions: []model.Decision{{
			At: base, SessionKey: "5f2c0b7d", Model: "claude-fable-5", Provider: "claude",
			ChosenAuthID: "auth-a", PreviousAuthID: "auth-b", Kind: model.DecisionFailover,
			Note: "bound credential was not offered", Subagent: true,
			Scores: []model.Score{{
				AuthID: "auth-b", Cost: -0.9,
				Eligible: false, Reason: model.ReasonSpent,
				Windows: []model.WindowScore{{
					Kind: model.WindowWeekly, Elapsed: 0.9, Target: 1,
					Utilization: 1, Slack: 0, Weight: 1,
					ResetsAt: base.Add(16 * time.Hour),
				}},
			}},
		}},
		// One of the warnings runtime.Status actually emits, paired with the
		// snapshot error that produces it.
		Warnings: []string{"quota poll failing for auth-a (timeout): boom"},
	}
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	src := &stubSource{status: richStatus()}
	h := testHandler(src)

	cases := []struct {
		method, target string
		wantCode       int
		wantType       string
	}{
		{http.MethodGet, "/", http.StatusOK, "text/html; charset=utf-8"},
		{http.MethodGet, "/index.html", http.StatusOK, "text/html; charset=utf-8"},
		{http.MethodHead, "/", http.StatusOK, "text/html; charset=utf-8"},
		{http.MethodGet, "/api/status", http.StatusOK, "application/json; charset=utf-8"},
		{http.MethodGet, "/api/status?model=x", http.StatusOK, "application/json; charset=utf-8"},
		{http.MethodGet, "/api/", http.StatusNotFound, ""},
		{http.MethodGet, "/api/status/extra", http.StatusNotFound, ""},
		{http.MethodGet, "/index.htm", http.StatusNotFound, ""},
		{http.MethodGet, "/nope", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
		if rec.Code != c.wantCode {
			t.Errorf("%s %s: code = %d, want %d", c.method, c.target, rec.Code, c.wantCode)
		}
		if c.wantType != "" && rec.Header().Get("Content-Type") != c.wantType {
			t.Errorf("%s %s: content type = %q, want %q",
				c.method, c.target, rec.Header().Get("Content-Type"), c.wantType)
		}
	}

	// A rejected method must be refused before the Source is consulted.
	rejected := &stubSource{status: richStatus()}
	rh := testHandler(rejected)
	for _, c := range []struct{ method, target string }{
		{http.MethodPost, "/api/status"},
		{http.MethodDelete, "/index.html"},
		{http.MethodPut, "/api/status"},
		{http.MethodPatch, "/index.html"},
	} {
		rec := httptest.NewRecorder()
		rh.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: code = %d, want %d", c.method, c.target, rec.Code, http.StatusMethodNotAllowed)
		}
	}
	if rejected.calls != 0 {
		t.Errorf("Source called %d times for rejected methods, want 0", rejected.calls)
	}
}

func TestHeaders(t *testing.T) {
	t.Parallel()
	h := testHandler(&stubSource{status: richStatus()})
	for _, target := range []string{"/", "/index.html", "/api/status", "/missing"} {
		rec := get(t, h, target)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", target, got)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if csp == "" {
			t.Fatalf("%s: no Content-Security-Policy", target)
		}
		// Every directive by name: a policy that lost one would otherwise
		// still pass a default-deny check.
		for _, want := range []string{
			"default-src 'none'",
			"style-src 'unsafe-inline'",
			"connect-src 'self'",
			"base-uri 'none'",
			"form-action 'none'",
			"frame-ancestors 'self'",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP is missing %q: %q", target, want, csp)
			}
		}
		// The inline block is named by hash, so the policy actually contains
		// script execution rather than waving it through.
		if !strings.Contains(csp, "script-src 'sha256-") {
			t.Errorf("%s: script-src does not name the inline block by hash: %q", target, csp)
		}
		if strings.Contains(csp, "script-src 'unsafe-inline'") {
			t.Errorf("%s: script-src falls back to unsafe-inline: %q", target, csp)
		}
		if strings.Contains(csp, "img-src") {
			t.Errorf("%s: CSP grants img-src, which the document has no use for: %q", target, csp)
		}
		for _, banned := range []string{"http:", "https:", "*"} {
			if strings.Contains(csp, banned) {
				t.Errorf("%s: CSP allows a remote origin (%q): %q", target, banned, csp)
			}
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", target, got)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/status", nil))
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

func TestStatusJSONRoundTrip(t *testing.T) {
	t.Parallel()
	want := richStatus()
	h := testHandler(&stubSource{status: want})

	rec := get(t, h, "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	// This route serves the pace curve and the poll cadence for the whole
	// config and a hashed credential id; everything else travels unchanged.
	expect := want
	expect.Config = model.Config{
		Pace:  want.Config.Pace,
		Quota: model.QuotaConfig{PollInterval: want.Config.Quota.PollInterval},
	}
	expect.Auths = []model.AuthStatus{want.Auths[0]}
	expect.Auths[0].AuthID = testID("auth-a")
	expect.Auths[0].Snapshot.AuthID = testID("auth-a")
	expect.Auths[0].Snapshot.AuthIndex = ""
	expect.Auths[0].Score.AuthID = testID("auth-a")
	expect.Bindings = []model.Binding{want.Bindings[0]}
	expect.Bindings[0].AuthID = testID("auth-a")
	expect.Decisions = []model.Decision{want.Decisions[0]}
	expect.Decisions[0].ChosenAuthID = testID("auth-a")
	expect.Decisions[0].PreviousAuthID = testID("auth-b")
	expect.Decisions[0].Scores = []model.Score{want.Decisions[0].Scores[0]}
	expect.Decisions[0].Scores[0].AuthID = testID("auth-b")
	expect.Warnings = []string{"quota poll failing for " + testID("auth-a") + " (timeout): boom"}
	if !reflect.DeepEqual(got, expect) {
		t.Errorf("round trip changed the status\n got: %+v\nwant: %+v", got, expect)
	}
	if strings.Contains(rec.Body.String(), model.DefaultUsageURL) {
		t.Errorf("the unauthenticated route serves quota.usage_url: %s", rec.Body.String())
	}
}

func TestStatusPassesModelAndNow(t *testing.T) {
	t.Parallel()
	src := &stubSource{status: richStatus()}
	h := testHandler(src)

	before := time.Now()
	get(t, h, "/api/status?model=claude-opus-4-6")
	if src.gotModl != "claude-opus-4-6" {
		t.Errorf("model = %q, want claude-opus-4-6", src.gotModl)
	}
	if src.gotNow.Before(before) || src.gotNow.After(time.Now()) {
		t.Errorf("now = %v, outside the call window", src.gotNow)
	}

	get(t, h, "/api/status")
	if src.gotModl != "" {
		t.Errorf("empty model = %q, want the source default", src.gotModl)
	}

	get(t, h, "/api/status?model=a%2Fb+c")
	if src.gotModl != "a/b c" {
		t.Errorf("decoded model = %q, want %q", src.gotModl, "a/b c")
	}
	if src.calls != 3 {
		t.Errorf("Source called %d times, want 3", src.calls)
	}
}

// TestPageUndoesHostEscaping holds the page's entity table to the escaping the
// host applies to every string in a management JSON body, html.EscapeString:
// an entity the page does not restore would show as literal markup, and one it
// restores that the host never produced would corrupt a name holding that text.
func TestPageUndoesHostEscaping(t *testing.T) {
	t.Parallel()
	page := string(indexHTML)
	for _, c := range []string{"&", "<", ">", `"`, "'"} {
		esc := html.EscapeString(c)
		if esc == c {
			t.Fatalf("html.EscapeString leaves %q alone; the page's table is stale", c)
		}
		if entry := strconv.Quote(esc) + ": " + strconv.Quote(c); !strings.Contains(page, entry) {
			t.Errorf("page does not map %s back to %s (want %s)", esc, c, entry)
		}
		if name := strings.Trim(esc, "&;"); !strings.Contains(page, "|"+name+"|") && !strings.Contains(page, "(?:"+name+"|") && !strings.Contains(page, "|"+name+");") {
			t.Errorf("page's entity pattern does not match %s", esc)
		}
	}
}

func TestPageIsOffline(t *testing.T) {
	t.Parallel()
	page := get(t, testHandler(&stubSource{status: richStatus()}), "/").Body.String()

	for _, banned := range []string{"http://", "https://", "//fonts.", "cdn."} {
		if strings.Contains(page, banned) {
			t.Errorf("page references a remote origin: %q", banned)
		}
	}
	// A root-absolute API path would break under the host's mount prefix.
	rootAbs := regexp.MustCompile(`["'(\x60]/(api|index\.html|static|assets)`)
	if m := rootAbs.FindString(page); m != "" {
		t.Errorf("page uses a root-absolute URL: %q", m)
	}
	// On the host the data comes from the page-status management route, found
	// from the page's own mount; the relative api/status serves cmd/webdev.
	for _, want := range []string{`"/v0/management/plugins/"`, `"/page-status"`, `: "api/status";`, `DATA_URL + "?model="`} {
		if !strings.Contains(page, want) {
			t.Errorf("page does not build its data URL from %s", want)
		}
	}
	if strings.Contains(page, "<script src") || strings.Contains(page, "<link rel=\"stylesheet\"") {
		t.Error("page loads an external script or stylesheet")
	}
}

func TestPageHasElementsTheScriptNeeds(t *testing.T) {
	t.Parallel()
	page := get(t, testHandler(&stubSource{status: richStatus()}), "/index.html").Body.String()

	ids := []string{
		"tooltip", "svg-ns",
		"page-foot", "plugin-facts", "next-pick", "seat-total", "seat-eligible", "snapshot-age",
		"refresh-toggle", "theme-toggle", "sync-now",
		"live-dot", "next-sync",
		"error-strip", "warnings", "loading", "empty-state", "fail-state", "fail-detail", "app",
		"sec-seats", "seats-sub",
		"timeline", "timeline-legend",
		"sec-pace", "pace-sub", "pace-curve", "pace-legend", "pace-missing", "pace-note",
		"pace-formula", "pace-rank", "pace-view",
		"sec-session", "session-sub", "session-chart", "session-legend", "session-missing", "session-focus", "session-note",
		"sec-hist", "hist-sub", "hist-view", "hist-span", "hist-live", "hist-range", "hist-chart", "hist-rank", "hist-legend",
		"sec-decisions", "decisions",
		"sec-bindings", "bindings",
	}
	for _, id := range ids {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("page is missing id=%q", id)
		}
	}
	for _, frag := range []string{
		`prefers-color-scheme: dark`,
		`aria-pressed`,
		`"dec-fam"`,
		`byFamily`,
	} {
		if !strings.Contains(page, frag) {
			t.Errorf("page is missing %q", frag)
		}
	}
}

// TestScriptHashCoversTheServedPage holds the policy's hash to the script the
// page actually carries: a drift between them stops the document running.
func TestScriptHashCoversTheServedPage(t *testing.T) {
	t.Parallel()
	page := get(t, testHandler(&stubSource{}), "/index.html").Body.Bytes()
	i := bytes.Index(page, []byte("<script>"))
	j := bytes.Index(page, []byte("</script>"))
	if i < 0 || j < i {
		t.Fatal("the page has no inline script block")
	}
	sum := sha256.Sum256(page[i+len("<script>") : j])
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(contentSecurityPolicy, want) {
		t.Errorf("policy does not cover the served script\n  policy: %s\n  want:   %s",
			contentSecurityPolicy, want)
	}
	if bytes.Count(page, []byte("<script")) != 1 {
		t.Error("the page carries more than one script, so one hash cannot cover it")
	}
}

// TestPageHoldsNoRewrittenByte covers the one way the shipped policy can name a
// script no browser runs: the HTML tokenizer rewrites NUL to U+FFFD and CR to
// LF inside script data, so a document carrying either hashes to one value here
// and to another in the browser, which then blocks the page whole.
func TestPageHoldsNoRewrittenByte(t *testing.T) {
	t.Parallel()
	page := get(t, testHandler(&stubSource{}), "/index.html").Body.Bytes()
	for _, c := range []struct {
		b    byte
		name string
	}{{0x00, "NUL"}, {'\r', "CR"}} {
		if i := bytes.IndexByte(page, c.b); i >= 0 {
			t.Errorf("page carries %s at byte %d, which the parser rewrites before it hashes", c.name, i)
		}
	}
}

// TestNonFiniteSurvivesEncoding covers a malformed upstream reading: a NaN or
// an infinity reaches the page as null rather than failing the whole endpoint
// and hiding every credential.
func TestNonFiniteSurvivesEncoding(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Auths[0].Snapshot.Windows[0].Utilization = math.NaN()
	st.Auths[0].Snapshot.Windows[1].Utilization = math.Inf(1)
	st.Auths[0].Score = model.Score{
		AuthID: "auth-a", Cost: math.NaN(),
		Reason: model.ReasonBadReading,
		Windows: []model.WindowScore{{
			Kind: model.WindowSession, Utilization: math.NaN(), Slack: math.NaN(),
		}},
	}
	st.Config.Pace.Steepness = math.NaN()
	st.Decisions[0].Scores[0].Cost = math.Inf(1)
	st.Decisions[0].Scores[0].Windows[0].Slack = math.NaN()

	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "NaN") || strings.Contains(body, "Inf") ||
		strings.Contains(body, string(nonFiniteLiteral)) {
		t.Errorf("a non-finite value reached the body: %s", body)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	auth := raw["auths"].([]any)[0].(map[string]any)
	win := auth["snapshot"].(map[string]any)["windows"].([]any)[0].(map[string]any)
	if win["utilization"] != nil {
		t.Errorf("utilization = %v, want null", win["utilization"])
	}
	if auth["score"].(map[string]any)["cost"] != nil {
		t.Errorf("score cost = %v, want null", auth["score"].(map[string]any)["cost"])
	}
	if raw["config"].(map[string]any)["pace"].(map[string]any)["steepness"] != nil {
		t.Error("steepness is not null")
	}
	// A decision carries its own candidate scores, on the same path.
	logged := raw["decisions"].([]any)[0].(map[string]any)["scores"].([]any)[0].(map[string]any)
	if logged["cost"] != nil {
		t.Errorf("logged score cost = %v, want null", logged["cost"])
	}
	if logged["windows"].([]any)[0].(map[string]any)["slack"] != nil {
		t.Error("logged window slack is not null")
	}
}

// TestSourceStateIsNotMutated covers the copies the reduction makes: the
// Source keeps handing out the values it was built with.
func TestSourceStateIsNotMutated(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Auths[0].Label = "ops@example.com"
	st.Auths[0].Snapshot.Windows[0].Utilization = math.NaN()
	src := &stubSource{status: st}

	get(t, testHandler(src), "/api/status")
	if src.status.Auths[0].Label != "ops@example.com" {
		t.Errorf("label was rewritten in place: %q", src.status.Auths[0].Label)
	}
	if !math.IsNaN(src.status.Auths[0].Snapshot.Windows[0].Utilization) {
		t.Error("the source's own window reading was rewritten in place")
	}
	if src.status.Config.Quota.UsageURL == "" {
		t.Error("the source's own config was rewritten in place")
	}
}

func TestPublicLabelMasksEmails(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"ops@example.com", "o…@example.com"},
		{"a@sub.example.co.uk", "a…@sub.example.co.uk"},
		{"Seat A", "Seat A"},
		{"claude-oauth-a4f1c2", "claude-oauth-a4f1c2"},
		{"prod @ us-east", "prod @ us-east"},
		{"@example.com", "@example.com"},
		{"ops@", "ops@"},
		{"", ""},
	}
	for _, c := range cases {
		if got := maskEmail(c.in); got != c.want {
			t.Errorf("maskEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFileNameResidue(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, email, provider, want string }{
		{"claude-alice-team-a.json", "alice@example.com", "claude", "team-a"},
		{"claude-alice-team-b.json", "alice@example.com", "claude", "team-b"},
		{"claude-ops@acme.example.json", "ops@acme.example", "claude", ""},
		{"claude-ops@acme.example.json", "", "claude", ""},
		{"claude-Ops@Acme.example-eu.json", "ops@acme.example", "claude", "eu"},
		{"claude-team-data.json", "svc.data@acme.example", "claude", "team-data"},
		{"claude-seat-b.json", "", "", "claude-seat-b"},
		{"claude-oauth-a4f1c2", "", "claude", "oauth-a4f1c2"},
		{"claude.json", "", "claude", "claude"},
		{"", "x@y.example", "claude", ""},
	}
	for _, c := range cases {
		if got := fileNameResidue(c.name, c.email, c.provider); got != c.want {
			t.Errorf("fileNameResidue(%q, %q, %q) = %q, want %q", c.name, c.email, c.provider, got, c.want)
		}
	}
}

// TestPublicSeatNamesAreDistinct covers the naming order and its uniqueness:
// two credentials of one account are told apart by what the operator put in
// the file name, and two whose masked addresses coincide by a tag of the id.
func TestPublicSeatNamesAreDistinct(t *testing.T) {
	t.Parallel()
	auths := []model.AuthStatus{
		{AuthID: "claude-alice-team-a.json", Label: "alice@example.com", Email: "alice@example.com", Name: "claude-alice-team-a.json", Provider: "claude"},
		{AuthID: "claude-alice-team-b.json", Label: "alice@example.com", Email: "alice@example.com", Name: "claude-alice-team-b.json", Provider: "claude"},
		{AuthID: "claude-ops@acme.example.json", Label: "ops@acme.example", Email: "ops@acme.example", Name: "claude-ops@acme.example.json", Provider: "claude"},
		{AuthID: "claude-oncall@acme.example.json", Label: "oncall@acme.example", Email: "oncall@acme.example", Name: "claude-oncall@acme.example.json", Provider: "claude"},
		{AuthID: "claude-seat-e.json", Label: "Seat E", Name: "claude-seat-e.json", Provider: "claude"},
		{AuthID: "claude-lone@acme.example.json", Label: "lone@acme.example", Name: "claude-lone@acme.example.json", Provider: "claude"},
		{AuthID: "bare"},
	}
	got := publicSeatLabels(auths, testID)
	want := []string{
		"team-a", "team-b",
		"o…@acme.example #" + testID(auths[2].AuthID)[:seatTagLen],
		"o…@acme.example #" + testID(auths[3].AuthID)[:seatTagLen],
		"Seat E", "l…@acme.example", "",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	seen := map[string]int{}
	for i, n := range got {
		if n == "" {
			continue
		}
		if j, dup := seen[n]; dup {
			t.Errorf("rows %d and %d share the name %q", j, i, n)
		}
		seen[n] = i
	}
	for _, n := range got {
		if strings.Contains(n, "yuya") || strings.Contains(n, "ops@") || strings.Contains(n, "oncall") || strings.Contains(n, "lone@") {
			t.Errorf("a published name carries the account's local part: %q", n)
		}
	}
}

// TestSeatIdentityIsReduced covers the route for the identity fields: the file
// name is withheld, the address is masked, and the label is the published
// name on both the row and its snapshot.
func TestSeatIdentityIsReduced(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Auths[0].Label = "quota.bot@acme-corp.example"
	st.Auths[0].Email = "quota.bot@acme-corp.example"
	st.Auths[0].Name = "claude-quota.bot@acme-corp.example-primary.json"
	st.Auths[0].Snapshot.Label = "quota.bot@acme-corp.example"

	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	if body := rec.Body.String(); strings.Contains(body, "quota.bot") {
		t.Errorf("the account's local part is served on the unauthenticated route: %s", body)
	}
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := got.Auths[0]
	if a.Label != "primary" || a.Snapshot.Label != "primary" {
		t.Errorf("labels = %q / %q, want the file name residue on both", a.Label, a.Snapshot.Label)
	}
	if a.Name != "" {
		t.Errorf("name = %q, want it withheld", a.Name)
	}
	if a.Email != "q…@acme-corp.example" {
		t.Errorf("email = %q, want it masked", a.Email)
	}
}

// TestConfigIsReducedToThePaceCurve covers the config reduction: the page reads
// the curve, and the rest of the block is operator configuration an anonymous
// reader has no use for.
func TestConfigIsReducedToThePaceCurve(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Config.Quota.UsageURL = "https://usage.internal.example/api/oauth/usage"
	st.Config.Quota.PollInterval = 2 * time.Minute

	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Config.Pace != st.Config.Pace {
		t.Errorf("pace = %+v, want %+v", got.Config.Pace, st.Config.Pace)
	}
	want := model.Config{
		Pace:  st.Config.Pace,
		Quota: model.QuotaConfig{PollInterval: st.Config.Quota.PollInterval},
	}
	if !reflect.DeepEqual(got.Config, want) {
		t.Errorf("the route serves config beyond the pace curve and poll cadence: %+v", got.Config)
	}
	if strings.Contains(rec.Body.String(), "usage.internal.example") {
		t.Errorf("the configured usage endpoint reached the body: %s", rec.Body.String())
	}
}

// TestWarningsDropURLs covers the operator warnings: a failing poll quotes the
// transport error, which names the endpoint the config reduction withholds.
func TestWarningsDropURLs(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Warnings = []string{
		`quota poll failing for auth-a (timeout): Get "https://usage.internal.example/api/oauth/usage": context deadline exceeded`,
		"provider claude offered a single candidate; the pool shares one priority tier",
	}
	original := st.Warnings[0]
	src := &stubSource{status: st}

	rec := get(t, testHandler(src), "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := []string{
		`quota poll failing for ` + testID("auth-a") + ` (timeout): Get "…": context deadline exceeded`,
		"provider claude offered a single candidate; the pool shares one priority tier",
	}
	if !reflect.DeepEqual(got.Warnings, want) {
		t.Errorf("warnings = %q, want %q", got.Warnings, want)
	}
	if src.status.Warnings[0] != original {
		t.Errorf("the source's own warning was rewritten in place: %q", src.status.Warnings[0])
	}
}

// A host error that names the auth directory must not tell an anonymous
// reader where the credentials live.
func TestWarningsDropFilesystemPaths(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Warnings = []string{
		`credential listing is failing: open /home/op/.cli-proxy-api/auths: permission denied`,
		`quota poll failing for auth-a (auth): read "/var/lib/cpa/auths/claude-a.json": no such file`,
	}
	src := &stubSource{status: st}

	rec := get(t, testHandler(src), "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, w := range got.Warnings {
		if strings.Contains(w, "/home/") || strings.Contains(w, "/var/") || strings.Contains(w, ".cli-proxy-api") {
			t.Errorf("warnings[%d] still names a path: %q", i, w)
		}
	}
	if !strings.Contains(got.Warnings[0], "permission denied") {
		t.Errorf("warnings[0] lost its error text: %q", got.Warnings[0])
	}
}

// Two seats with no id and no name still get distinct labels, and the empty
// id does not panic the tag.
func TestPublicSeatLabelsSurviveAnEmptyID(t *testing.T) {
	t.Parallel()
	got := publicSeatLabels([]model.AuthStatus{{AuthID: ""}, {AuthID: ""}}, testID)
	if len(got) != 2 || got[0] == "" || got[1] == "" || got[0] == got[1] {
		t.Errorf("labels for two empty ids = %q, want two distinct non-empty labels", got)
	}
}

// A snapshot that exists with no windows ships an empty list, like every
// other list on the wire, so the page's "no windows" branch matches
// production rather than a fixture.
func TestSnapshotWithNoWindowsShipsAnEmptyList(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Auths[0].Snapshot = model.AuthSnapshot{AuthID: st.Auths[0].AuthID, ObservedAt: base}
	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	auth := got["auths"].([]any)[0].(map[string]any)
	win, ok := auth["snapshot"].(map[string]any)["windows"]
	if !ok || win == nil {
		t.Fatalf("snapshot.windows = %v, want an empty list", win)
	}
	if l, isList := win.([]any); !isList || len(l) != 0 {
		t.Errorf("snapshot.windows = %v, want []", win)
	}
}

// TestBindingsAreBounded covers the payload cap: the store runs to
// affinity.max-sessions and this route re-encodes it every poll.
func TestBindingsAreBounded(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Bindings = make([]model.Binding, maxStatusBindings+40)
	for i := range st.Bindings {
		st.Bindings[i] = model.Binding{SessionKey: "s" + strconv.Itoa(i), AuthID: "auth-a"}
	}
	src := &stubSource{status: st}

	rec := get(t, testHandler(src), "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Bindings) != maxStatusBindings {
		t.Errorf("bindings = %d, want %d", len(got.Bindings), maxStatusBindings)
	}
	if len(src.status.Bindings) != maxStatusBindings+40 {
		t.Errorf("the source's own list was truncated: %d", len(src.status.Bindings))
	}
	var told bool
	for _, w := range got.Warnings {
		if strings.Contains(w, "truncated") && strings.Contains(w, strconv.Itoa(maxStatusBindings+40)) {
			told = true
		}
	}
	if !told {
		t.Errorf("truncation is not reported to the operator: %q", got.Warnings)
	}

	// A list within the cap is served whole and says nothing about truncation.
	small := richStatus()
	whole := get(t, testHandler(&stubSource{status: small}), "/api/status").Body.String()
	if strings.Contains(whole, "truncated") {
		t.Errorf("an untruncated list reports truncation: %s", whole)
	}
}

func TestEmptyStatusStillServes(t *testing.T) {
	t.Parallel()
	h := testHandler(&stubSource{})
	rec := get(t, h, "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Auths) != 0 {
		t.Errorf("auths = %d, want 0", len(got.Auths))
	}
}

// seatAID and seatBID are the shape the substitution exists for: CLIProxyAPI
// names a Claude OAuth credential file after the account and falls back to
// that name for the id, and these two mask to one label.
const (
	seatAID = "alice@acme.example.json"
	seatBID = "aaron@acme.example.json"
)

// idStatus carries seatAID and seatBID through every field of model.Status
// that references a credential, including the two that name one in free text.
func idStatus() model.Status {
	return model.Status{
		Now:   base,
		Model: "claude-fable-5",
		Auths: []model.AuthStatus{{
			AuthID: seatAID, Label: "alice@acme.example", Provider: "claude", HostStatus: "active",
			Snapshot: model.AuthSnapshot{
				AuthID: seatAID, AuthIndex: "3011c15be15be4ee", Label: "alice@acme.example",
				ObservedAt: base, Source: model.SourceUsageEndpoint, Windows: []model.Window{},
			},
			Score: model.Score{AuthID: seatAID, Eligible: true},
		}, {
			AuthID: seatBID, Label: "aaron@acme.example", Provider: "claude", HostStatus: "active",
			Snapshot: model.AuthSnapshot{
				AuthID: seatBID, AuthIndex: "8c41f0a2b7d5e693", Label: "aaron@acme.example",
				ObservedAt: base, Source: model.SourceResponseHeaders, Windows: []model.Window{},
			},
			Score: model.Score{AuthID: seatBID, Reason: model.ReasonSpent},
		}},
		Bindings: []model.Binding{
			{SessionKey: "5f2c0b7d", Provider: "claude", Model: "claude-fable-5", AuthID: seatAID},
			{SessionKey: "a91d33e0", Provider: "claude", Model: "claude-fable-5", AuthID: seatBID},
		},
		Decisions: []model.Decision{{
			At: base, SessionKey: "5f2c0b7d", Model: "claude-fable-5", Provider: "claude",
			ChosenAuthID: seatAID, PreviousAuthID: seatBID, Kind: model.DecisionFailover,
			Note: "retry after " + seatBID,
			Scores: []model.Score{
				{AuthID: seatAID, Eligible: true},
				{AuthID: seatBID, Reason: model.ReasonSpent},
			},
		}},
		Warnings: []string{seatBID + " has not been read yet; it cannot take a new conversation"},
	}
}

// TestCredentialIDsArePublished covers the correlation key: every table on the
// page joins credentials on the id, so the published form has to be injective
// and has to reach every field and every message that names one.
func TestCredentialIDsArePublished(t *testing.T) {
	t.Parallel()
	src := &stubSource{status: idStatus()}
	rec := get(t, testHandler(src), "/api/status")

	// The whole body, so a field added to model.Status and left carrying a
	// real id fails here rather than at the fields this test enumerates.
	body := rec.Body.String()
	for _, raw := range []string{seatAID, seatBID, "alice@", "aaron@", "3011c15be15be4ee"} {
		if strings.Contains(body, raw) {
			t.Errorf("the unauthenticated route serves %q: %s", raw, body)
		}
	}

	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	a, b := testID(seatAID), testID(seatBID)
	if a == b {
		t.Fatalf("two credentials share the published id %q", a)
	}
	shape := regexp.MustCompile(`^[0-9a-f]{` + strconv.Itoa(2*publicIDBytes) + `}$`)
	for _, id := range []string{a, b} {
		if !shape.MatchString(id) {
			t.Errorf("published id %q is not %d hex characters", id, 2*publicIDBytes)
		}
	}

	for _, c := range []struct{ where, got, want string }{
		{"auths[0].auth_id", got.Auths[0].AuthID, a},
		{"auths[0].snapshot.auth_id", got.Auths[0].Snapshot.AuthID, a},
		{"auths[0].score.auth_id", got.Auths[0].Score.AuthID, a},
		{"auths[1].auth_id", got.Auths[1].AuthID, b},
		{"auths[1].snapshot.auth_id", got.Auths[1].Snapshot.AuthID, b},
		{"auths[1].score.auth_id", got.Auths[1].Score.AuthID, b},
		{"bindings[0].auth_id", got.Bindings[0].AuthID, a},
		{"bindings[1].auth_id", got.Bindings[1].AuthID, b},
		{"decisions[0].chosen_auth_id", got.Decisions[0].ChosenAuthID, a},
		{"decisions[0].previous_auth_id", got.Decisions[0].PreviousAuthID, b},
		{"decisions[0].scores[0].auth_id", got.Decisions[0].Scores[0].AuthID, a},
		{"decisions[0].scores[1].auth_id", got.Decisions[0].Scores[1].AuthID, b},
		{"decisions[0].note", got.Decisions[0].Note, "retry after " + b},
		{"warnings[0]", got.Warnings[0], b + " has not been read yet; it cannot take a new conversation"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.where, c.got, c.want)
		}
	}
	for i, row := range got.Auths {
		if row.Snapshot.AuthIndex != "" {
			t.Errorf("auths[%d].snapshot.auth_index = %q, want the host index withheld", i, row.Snapshot.AuthIndex)
		}
	}
	if src.status.Auths[0].AuthID != seatAID || src.status.Bindings[0].AuthID != seatAID {
		t.Error("the source's own ids were rewritten in place")
	}
}

// TestEveryAuthIDFieldIsPublished walks the served JSON rather than the fields
// this package names, so a field whose key ends in auth_id and whose value is
// not a published id fails here whenever it is added.
func TestEveryAuthIDFieldIsPublished(t *testing.T) {
	t.Parallel()
	rec := get(t, testHandler(&stubSource{status: idStatus()}), "/api/status")
	var raw any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	published := map[string]bool{testID(seatAID): true, testID(seatBID): true}

	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch node := v.(type) {
		case map[string]any:
			for key, child := range node {
				at := path + "." + key
				if s, ok := child.(string); ok && strings.HasSuffix(key, "auth_id") {
					if s != "" && !published[s] {
						t.Errorf("%s = %q, want a published credential id", at, s)
					}
					continue
				}
				walk(at, child)
			}
		case []any:
			for i, child := range node {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("status", raw)
}

// TestSnapshotErrorDropsURLs covers the snapshot the page reads a failing poll
// off: the transport error quotes the usage endpoint the config reduction
// withholds.
func TestSnapshotErrorDropsURLs(t *testing.T) {
	t.Parallel()
	st := idStatus()
	st.Auths[0].Snapshot.Err = `Get "https://usage.internal.example/api/oauth/usage": context deadline exceeded`
	st.Auths[0].Snapshot.ErrCategory = "timeout"

	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := `Get "…": context deadline exceeded`; got.Auths[0].Snapshot.Err != want {
		t.Errorf("snapshot err = %q, want %q", got.Auths[0].Snapshot.Err, want)
	}
	if got.Auths[0].Snapshot.ErrCategory != "timeout" {
		t.Errorf("err category = %q, want it carried through", got.Auths[0].Snapshot.ErrCategory)
	}
}

// TestFreeTextMasksAnUnlistedCredential covers the decision that outlives the
// credential row it names: the note is retained past the poll that drops the
// row, so the id in it no longer matches any credential the status carries.
func TestFreeTextMasksAnUnlistedCredential(t *testing.T) {
	t.Parallel()
	st := idStatus()
	st.Decisions[0].Note = "retry after removed@acme.example.json"

	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	if strings.Contains(rec.Body.String(), "removed@") {
		t.Errorf("an unlisted credential's address is served: %s", rec.Body.String())
	}
}

// syncSource is a Source that can also be asked for a fresh upstream read.
type syncSource struct {
	stubSource
	syncs int
}

func (s *syncSource) SyncNow(context.Context) bool { s.syncs++; return true }

// The page asks for a fresh provider read with sync=1, and only then. A source
// with no SyncNow serves the request rather than failing it.
func TestStatusSyncsOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	src := &syncSource{stubSource: stubSource{status: richStatus()}}
	h := testHandler(src)

	if rec := get(t, h, "/api/status"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if src.syncs != 0 {
		t.Errorf("syncs = %d, want none without sync=1", src.syncs)
	}

	if rec := get(t, h, "/api/status?sync=1"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if src.syncs != 1 {
		t.Errorf("syncs = %d, want exactly one", src.syncs)
	}
	if src.calls != 2 {
		t.Errorf("Status calls = %d, want one per request", src.calls)
	}

	plain := testHandler(&stubSource{status: richStatus()})
	if rec := get(t, plain, "/api/status?sync=1"); rec.Code != http.StatusOK {
		t.Errorf("a source without SyncNow answered %d, want 200", rec.Code)
	}
}

// TestNonFiniteHistorySampleSurvivesEncoding covers a recorded sample that is
// not finite: Sample.MarshalJSON refuses one outright, so without sanitising
// the history a single bad sample fails the whole response.
func TestNonFiniteHistorySampleSurvivesEncoding(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Auths[0].History[0].Cycles[0].Samples[0].Utilization = math.NaN()
	st.Auths[0].History[0].Cycles[0].Samples[1].Utilization = math.Inf(1)

	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	auth := raw["auths"].([]any)[0].(map[string]any)
	samples := auth["history"].([]any)[0].(map[string]any)["cycles"].([]any)[0].(map[string]any)["samples"].([]any)
	for i, s := range samples {
		if v := s.([]any)[1]; v != nil {
			t.Errorf("sample %d utilization = %v, want null", i, v)
		}
	}
	if got := st.Auths[0].History[0].Cycles[0].Samples[0].Utilization; !math.IsNaN(got) {
		t.Errorf("source history was mutated: %v", got)
	}
}

// TestFileNameResidueFoldsWithoutRealigning covers letters whose lowercase has
// another UTF-8 length: U+023A grows by a byte and the Kelvin sign shrinks by
// two, so a match found in a lowercased copy lands at the wrong offset in the
// name itself.
func TestFileNameResidueFoldsWithoutRealigning(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, email, want string }{
		{"claude-\u212a\u212abob.json", "bob@acme.example", "\u212a\u212a"},
		{"claude-\u212aim-eu.json", "kim@acme.example", "eu"},
		{"\u023a\u023a\u023a\u023a-bob.json", "bob@acme.example", "\u023a\u023a\u023a\u023a"},
	}
	for _, c := range cases {
		if got := fileNameResidue(c.name, c.email, "claude"); got != c.want {
			t.Errorf("fileNameResidue(%q, %q) = %q, want %q", c.name, c.email, got, c.want)
		}
	}

	st := richStatus()
	st.Auths[0].Label = "bob@acme.example"
	st.Auths[0].Email = "bob@acme.example"
	st.Auths[0].Name = "\u023a\u023a\u023a\u023a-bob.json"
	rec := get(t, testHandler(&stubSource{status: st}), "/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bob") {
		t.Errorf("the account's local part is served: %s", rec.Body.String())
	}
}

// TestPublicTextDropsHostsAndPaths covers what an os or transport error names
// beyond a URL and a Unix path: a Windows path, a home-relative path, and the
// address or host name a dial, a lookup or a certificate check quotes.
func TestPublicTextDropsHostsAndPaths(t *testing.T) {
	t.Parallel()
	ids := strings.NewReplacer()
	cases := []struct{ in, want string }{
		{`open C:\Users\jane.doe\.cli-proxy-api\auths: Access is denied.`, `open …: Access is denied.`},
		{`open C:\Users\Jane Doe\.cli-proxy-api\auths\claude.json: The system cannot find the file specified.`, `open …: The system cannot find the file specified.`},
		{`read "C:/Users/jane.doe/auths/claude.json" (auth)`, `read "…" (auth)`},
		{`open \\fileserver\share\auths\claude.json: Access is denied.`, `open …: Access is denied.`},
		{`open \\?\C:\Users\jane.doe\auths: Access is denied.`, `open …: Access is denied.`},
		{`open ~/.cli-proxy-api/auths: permission denied`, `open …: permission denied`},
		{`stat ~jane/auths/claude.json: no such file`, `stat …: no such file`},
		{`dial tcp 10.1.2.3:443: connect: connection refused`, `dial tcp …: connect: connection refused`},
		{`dial tcp: lookup usage.corp.internal on 192.168.1.1:53: no such host`, `dial tcp: lookup … on …: no such host`},
		{`dial tcp: lookup usage.corp.internal: no such host`, `dial tcp: lookup …: no such host`},
		{`dial tcp [::1]:8080: connect: connection refused`, `dial tcp …: connect: connection refused`},
		{`dial tcp [fe80::1%en0]:443: i/o timeout`, `dial tcp …: i/o timeout`},
		{`read tcp 192.168.1.5:52341->104.18.1.1:443: read: connection reset by peer`, `read tcp …->…: read: connection reset by peer`},
		{`proxyconnect tcp: dial tcp proxy.corp.internal:3128: i/o timeout`, `proxyconnect tcp: dial tcp …: i/o timeout`},
		{`dial tcp localhost:8317: connect: connection refused`, `dial tcp …: connect: connection refused`},
		{`tls: failed to verify certificate: x509: certificate is valid for *.corp.internal, proxy.corp.internal, not usage.corp.internal`, `tls: failed to verify certificate: x509: certificate is valid for …`},
		{`x509: certificate is not valid for any names, but wanted to match usage.corp.internal`, `x509: certificate is not valid for any names, but wanted to match …`},
	}
	for _, c := range cases {
		if got := publicText(c.in, ids); got != c.want {
			t.Errorf("publicText(%q)\n got: %q\nwant: %q", c.in, got, c.want)
		}
	}
}

// TestPublicTextKeepsPluginText covers the text the plugin itself writes into
// a warning, a snapshot error and a decision note, none of which names a host
// or a path: durations, versions, model ids and config keys read as written.
func TestPublicTextKeepsPluginText(t *testing.T) {
	t.Parallel()
	ids := strings.NewReplacer()
	for _, in := range []string{
		"quota: rate-limited (http 429): usage endpoint throttled",
		"quota: auth (http 401): credential rejected",
		"quota: bad-status (http 302): redirect refused",
		"quota: timeout: context deadline exceeded",
		"quota: transport: net/http: TLS handshake timeout",
		"quota: transport: http2: server sent GOAWAY and closed the connection",
		"usage request panicked: runtime error: index out of range [3] with length 3",
		"retry in 2m30s after 1h0m0s idle",
		"host v7.3.15 declared schema 1; plugin 0.1.0 built with go1.25.14",
		"model claude-opus-4-6-20260212 is not governed; claude-fable-5 is",
		"bound credential was not offered; retry after seat-b",
		"no session key; not pinned",
		"pinned to parent session",
		"provider claude: the host offers only seat-a (priority 10); the fallback tier (seat-b) takes no new conversation until the top tier runs out.",
		"plugin is disabled by configuration; the host's own selector routes every request",
		"run with routing.session-affinity: false and quota.poll-interval at 2m",
		"resets at 2026-09-04T15:04:05Z, 15:04 local",
		"binding list truncated to 500 of 540 entries for this view",
		"the usage endpoint is api.anthropic.com",
	} {
		if got := publicText(in, ids); got != in {
			t.Errorf("publicText(%q) = %q, want it unchanged", in, got)
		}
	}
}
