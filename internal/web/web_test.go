package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
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

// richStatus exercises every field the app reads, including the ones that only
// appear on an unhappy path.
func richStatus() model.Status {
	base := time.Date(2026, 9, 4, 15, 4, 5, 0, time.UTC)
	cfg := model.Defaults()
	cfg.Pace.CurveExponent = 1.35
	return model.Status{
		Now: base,
		Plugin: model.PluginInfo{
			Name:              "cpa-claude-quota-scheduler",
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
				AuthID: "auth-a", Total: -0.31, RawPenalty: 0.2, Eligible: true,
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
				AuthID: "auth-b", Total: -0.9, RawPenalty: 0.25,
				Eligible: false, Reason: model.ReasonHardCutoff,
				Windows: []model.WindowScore{{
					Kind: model.WindowWeekly, Elapsed: 0.9, Target: 0.88,
					Utilization: 0.99, Slack: -0.11, Weight: 1,
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
	h := NewHandler(src)

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
	rh := NewHandler(rejected)
	for _, c := range []struct{ method, target string }{
		{http.MethodPost, "/api/status"},
		{http.MethodPost, "/"},
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
	h := NewHandler(&stubSource{status: richStatus()})
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
			"frame-ancestors 'none'",
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
	h := NewHandler(&stubSource{status: want})

	rec := get(t, h, "/api/status")
	var got model.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	// This route drops the configured usage endpoint; everything else travels
	// unchanged.
	expect := want
	expect.Config.Quota.UsageURL = ""
	if !reflect.DeepEqual(got, expect) {
		t.Errorf("round trip changed the status\n got: %+v\nwant: %+v", got, expect)
	}
	if strings.Contains(rec.Body.String(), model.DefaultUsageURL) {
		t.Errorf("the unauthenticated route serves quota.usage_url: %s", rec.Body.String())
	}
	// Duration travels as nanoseconds, which the page converts itself.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	auths := raw["auths"].([]any)
	win := auths[0].(map[string]any)["snapshot"].(map[string]any)["windows"].([]any)[0].(map[string]any)
	if win["duration"].(float64) != float64(model.SessionDuration) {
		t.Errorf("duration = %v, want %v ns", win["duration"], float64(model.SessionDuration))
	}
}

func TestStatusPassesModelAndNow(t *testing.T) {
	t.Parallel()
	src := &stubSource{status: richStatus()}
	h := NewHandler(src)

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

func TestPageIsOffline(t *testing.T) {
	t.Parallel()
	page := get(t, NewHandler(&stubSource{status: richStatus()}), "/").Body.String()

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
	if !strings.Contains(page, `"api/status"`) {
		t.Error("page does not fetch the relative api/status")
	}
	if strings.Contains(page, "<script src") || strings.Contains(page, "<link rel=\"stylesheet\"") {
		t.Error("page loads an external script or stylesheet")
	}
}

func TestPageHasElementsTheScriptNeeds(t *testing.T) {
	t.Parallel()
	page := get(t, NewHandler(&stubSource{status: richStatus()}), "/index.html").Body.String()

	ids := []string{
		"tooltip", "svg-ns",
		"plugin-name", "plugin-version", "host-schema", "started-at", "next-pick",
		"model-input", "model-list", "refresh-toggle", "theme-toggle",
		"live-dot", "last-updated",
		"error-strip", "warnings", "loading", "empty-state", "fail-state", "fail-detail", "app",
		"sec-seats", "seats",
		"sec-timeline", "timeline", "timeline-legend",
		"sec-pace", "pace-curve", "pace-note",
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
		`<datalist`,
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
	page := get(t, NewHandler(&stubSource{}), "/index.html").Body.Bytes()
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
	page := get(t, NewHandler(&stubSource{}), "/index.html").Body.Bytes()
	for _, c := range []struct {
		b    byte
		name string
	}{{0x00, "NUL"}, {'\r', "CR"}} {
		if i := bytes.IndexByte(page, c.b); i >= 0 {
			t.Errorf("page carries %s at byte %d, which the parser rewrites before it hashes", c.name, i)
		}
	}
}

// TestNonFiniteSentinelLiteral holds nonFiniteLiteral to what the encoder
// actually writes for the sentinel.
func TestNonFiniteSentinelLiteral(t *testing.T) {
	t.Parallel()
	got, err := json.Marshal(nonFiniteSentinel)
	if err != nil {
		t.Fatalf("marshal sentinel: %v", err)
	}
	if !bytes.Equal(got, nonFiniteLiteral) {
		t.Errorf("encoder writes %s, nonFiniteLiteral is %s", got, nonFiniteLiteral)
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
		AuthID: "auth-a", Total: math.NaN(), RawPenalty: math.Inf(-1),
		Reason: model.ReasonBadReading,
		Windows: []model.WindowScore{{
			Kind: model.WindowSession, Utilization: math.NaN(), Slack: math.NaN(),
		}},
	}
	st.Config.Pace.HardCutoff = math.NaN()

	rec := get(t, NewHandler(&stubSource{status: st}), "/api/status")
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
	if auth["score"].(map[string]any)["total"] != nil {
		t.Errorf("score total = %v, want null", auth["score"].(map[string]any)["total"])
	}
	if raw["config"].(map[string]any)["pace"].(map[string]any)["hard_cutoff"] != nil {
		t.Error("hard_cutoff is not null")
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

	get(t, NewHandler(src), "/api/status")
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
		if got := publicLabel(c.in); got != c.want {
			t.Errorf("publicLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestEmailLabelsAreMasked covers the route: the account address never reaches
// an anonymous reader, on the credential row or on its snapshot.
func TestEmailLabelsAreMasked(t *testing.T) {
	t.Parallel()
	st := richStatus()
	st.Auths[0].Label = "quota.bot@acme-corp.example"
	st.Auths[0].Snapshot.Label = "quota.bot@acme-corp.example"

	body := get(t, NewHandler(&stubSource{status: st}), "/api/status").Body.String()
	if strings.Contains(body, "quota.bot@") {
		t.Errorf("the account email is served on the unauthenticated route: %s", body)
	}
	if !strings.Contains(body, "q…@acme-corp.example") {
		t.Errorf("the masked label is missing: %s", body)
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

	rec := get(t, NewHandler(src), "/api/status")
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
	whole := get(t, NewHandler(&stubSource{status: small}), "/api/status").Body.String()
	if strings.Contains(whole, "truncated") {
		t.Errorf("an untruncated list reports truncation: %s", whole)
	}
}

func TestEmptyStatusStillServes(t *testing.T) {
	t.Parallel()
	h := NewHandler(&stubSource{})
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
