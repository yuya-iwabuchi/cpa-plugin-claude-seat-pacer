package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
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
		Warnings: []string{"host session affinity is still enabled"},
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
		{http.MethodPost, "/api/status", http.StatusMethodNotAllowed, ""},
		{http.MethodPost, "/", http.StatusMethodNotAllowed, ""},
		{http.MethodDelete, "/index.html", http.StatusMethodNotAllowed, ""},
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
		if !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP does not default-deny: %q", target, csp)
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
	if got := get(t, h, "/").Header().Get("Content-Length"); got != "" {
		// The page is written directly, so a stale length header would be a bug.
		t.Logf("page Content-Length = %q", got)
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the status\n got: %+v\nwant: %+v", got, want)
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
		"error-strip", "warnings", "loading", "empty-state", "app",
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
