package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// testToken stands in for a real OAuth access token. No error message, and no
// snapshot the plugin stores, may contain it.
const testToken = "sk-ant-oat01-TESTONLY-3f9c2b1a"

// doerFunc is the stub every test in this package fetches through. Nothing here
// opens a socket.
type doerFunc func(ctx context.Context, req Request) (Response, error)

func (f doerFunc) Do(ctx context.Context, req Request) (Response, error) {
	return f(ctx, req)
}

func respondWith(status int, body []byte) doerFunc {
	return func(context.Context, Request) (Response, error) {
		return Response{StatusCode: status, Body: body}, nil
	}
}

// newTestClient pins the observation instant so a snapshot's ObservedAt is
// assertable.
func newTestClient(d Doer, timeout time.Duration) *Client {
	c := NewClient(d, "https://usage.test/api/oauth/usage", timeout)
	c.now = func() time.Time { return testNow }
	return c
}

func TestClientFetchSendsTheExpectedRequest(t *testing.T) {
	var got Request
	client := newTestClient(doerFunc(func(_ context.Context, req Request) (Response, error) {
		got = req
		return Response{StatusCode: 200, Body: fixture(t, "usage_early_week.json")}, nil
	}), time.Second)

	if _, err := client.Fetch(context.Background(), "auth-1", "0", testToken); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got.Method != "GET" {
		t.Errorf("method = %q, want GET", got.Method)
	}
	if got.URL != "https://usage.test/api/oauth/usage" {
		t.Errorf("url = %q", got.URL)
	}
	want := map[string]string{
		"Authorization":  "Bearer " + testToken,
		"Content-Type":   "application/json",
		"anthropic-beta": "oauth-2025-04-20",
	}
	for name, value := range want {
		if got.Header[name] != value {
			t.Errorf("header %s = %q, want %q", name, got.Header[name], value)
		}
	}
	if got.Header["User-Agent"] == "" {
		t.Error("User-Agent header is empty")
	}
}

func TestClientFetchSuccess(t *testing.T) {
	client := newTestClient(respondWith(200, fixture(t, "usage_late_week.json")), time.Second)

	snap, err := client.Fetch(context.Background(), "auth-2", "1", testToken)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.AuthID != "auth-2" || snap.AuthIndex != "1" {
		t.Errorf("identity = (%q,%q), want (auth-2,1)", snap.AuthID, snap.AuthIndex)
	}
	if snap.Source != model.SourceUsageEndpoint {
		t.Errorf("source = %q, want %q", snap.Source, model.SourceUsageEndpoint)
	}
	if !snap.ObservedAt.Equal(testNow) {
		t.Errorf("observed_at = %v, want %v", snap.ObservedAt, testNow)
	}
	if snap.Err != "" || snap.ErrCategory != "" {
		t.Errorf("err = (%q,%q), want both empty", snap.Err, snap.ErrCategory)
	}

	session, ok := snap.Window(model.WindowSession, "")
	if !ok {
		t.Fatal("no session window")
	}
	if session.Utilization != 1.0 {
		t.Errorf("session utilization = %v, want 1", session.Utilization)
	}
	if session.Blocking() {
		t.Error("critical severity is an advisory level, not an observed refusal")
	}
	if session.Severity != model.SeverityCritical {
		t.Errorf("session severity = %q, want the endpoint's %q", session.Severity, model.SeverityCritical)
	}
	if fable, ok := snap.Window(model.WindowWeeklyScoped, "Fable"); !ok || fable.Utilization != 0.67 {
		t.Errorf("fable window = %+v, ok = %v", fable, ok)
	}
}

func TestClientFetchFailures(t *testing.T) {
	blocked := doerFunc(func(ctx context.Context, _ Request) (Response, error) {
		<-ctx.Done()
		return Response{}, ctx.Err()
	})

	tests := []struct {
		name    string
		doer    Doer
		timeout time.Duration
		want    ErrCategory
	}{
		{name: "unauthorized", doer: respondWith(401, []byte(`{"error":"invalid_token"}`)), want: CategoryAuth},
		{name: "forbidden", doer: respondWith(403, []byte(`{"error":"forbidden"}`)), want: CategoryForbidden},
		{name: "throttled", doer: respondWith(429, []byte(`{"error":"rate_limit"}`)), want: CategoryRateLimited},
		{name: "server error", doer: respondWith(500, []byte("upstream boom")), want: CategoryBadStatus},
		{name: "found", doer: respondWith(302, nil), want: CategoryBadStatus},
		{name: "moved permanently", doer: respondWith(301, nil), want: CategoryBadStatus},
		{name: "no content", doer: respondWith(204, nil), want: CategoryBadJSON},
		{name: "html error page", doer: respondWith(200, []byte("<html>nope</html>")), want: CategoryBadJSON},
		{name: "empty window set", doer: respondWith(200, []byte(`{}`)), want: CategoryBadJSON},
		{
			name: "oversize body",
			doer: respondWith(200, make([]byte, MaxBodyBytes+1)),
			want: CategoryOversize,
		},
		{
			name: "transport failure",
			doer: doerFunc(func(context.Context, Request) (Response, error) {
				return Response{}, errors.New("proxy dial refused")
			}),
			want: CategoryTransport,
		},
		{
			name: "deadline reported by the doer",
			doer: doerFunc(func(context.Context, Request) (Response, error) {
				return Response{}, fmt.Errorf("usage read: %w", context.DeadlineExceeded)
			}),
			want: CategoryTimeout,
		},
		{name: "client timeout", doer: blocked, timeout: time.Millisecond, want: CategoryTimeout},
		{name: "no doer", doer: nil, want: CategoryTransport},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(tc.doer, tc.timeout)
			snap, err := client.Fetch(context.Background(), "auth-3", "2", testToken)
			if err == nil {
				t.Fatalf("Fetch succeeded, want a %s failure", tc.want)
			}
			if got := Category(err); got != tc.want {
				t.Errorf("category = %q, want %q", got, tc.want)
			}
			if snap.Err != err.Error() {
				t.Errorf("snapshot err = %q, want %q", snap.Err, err.Error())
			}
			if snap.ErrCategory != string(tc.want) {
				t.Errorf("snapshot err_category = %q, want %q", snap.ErrCategory, tc.want)
			}
			if len(snap.Windows) != 0 {
				t.Errorf("windows = %+v, want none", snap.Windows)
			}
			if snap.AuthID != "auth-3" || snap.AuthIndex != "2" {
				t.Errorf("identity = (%q,%q), want (auth-3,2)", snap.AuthID, snap.AuthIndex)
			}
			assertNoToken(t, err.Error())
			assertNoToken(t, snap.Err)
		})
	}
}

// TestClientFetchTimesOutOnADoerThatIgnoresTheContext holds the deadline the
// poll loop depends on. The Doer here never looks at ctx, so only Fetch's own
// wait can stop the round trip.
func TestClientFetchTimesOutOnADoerThatIgnoresTheContext(t *testing.T) {
	const doerDelay = 2 * time.Second
	released := make(chan struct{})
	client := newTestClient(doerFunc(func(context.Context, Request) (Response, error) {
		time.Sleep(doerDelay)
		close(released)
		return Response{StatusCode: 200, Body: fixture(t, "usage_early_week.json")}, nil
	}), 20*time.Millisecond)

	started := time.Now()
	snap, err := client.Fetch(context.Background(), "auth-6", "5", testToken)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("Fetch succeeded, want a timeout")
	}
	if got := Category(err); got != CategoryTimeout {
		t.Errorf("category = %q, want %q", got, CategoryTimeout)
	}
	if snap.ErrCategory != string(CategoryTimeout) {
		t.Errorf("snapshot err_category = %q, want %q", snap.ErrCategory, CategoryTimeout)
	}
	if len(snap.Windows) != 0 {
		t.Errorf("windows = %+v, want none: the response arrived after the deadline", snap.Windows)
	}
	if elapsed >= doerDelay {
		t.Errorf("Fetch waited %v, want it to stop at its own deadline", elapsed)
	}

	// The abandoned goroutine finishes on its own and its result is dropped.
	<-released
}

// TestClientFetchStampsObservedAtAfterTheResponse pins the ordering Store.Put's
// per-window recency rests on: a merge that lands mid round-trip is newer than
// the reading the fetch brings back, not older.
func TestClientFetchStampsObservedAtAfterTheResponse(t *testing.T) {
	clock := testNow
	client := NewClient(doerFunc(func(context.Context, Request) (Response, error) {
		clock = clock.Add(30 * time.Second)
		return Response{StatusCode: 200, Body: fixture(t, "usage_early_week.json")}, nil
	}), "https://usage.test/api/oauth/usage", time.Second)
	client.now = func() time.Time { return clock }

	snap, err := client.Fetch(context.Background(), "auth-7", "6", testToken)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := testNow.Add(30 * time.Second); !snap.ObservedAt.Equal(want) {
		t.Errorf("observed_at = %v, want %v: the reading is dated when it arrived", snap.ObservedAt, want)
	}
}

// TestClientFetchNeverLeaksTheToken covers the failure paths that carry
// attacker-influenced or caller-supplied text: a Doer that quotes the request
// it was handed, and a body that echoes the credential back.
func TestClientFetchNeverLeaksTheToken(t *testing.T) {
	echoBody := []byte(`{"error":"token ` + testToken + ` is not permitted","five_hour":null}`)

	tests := []struct {
		name string
		doer Doer
	}{
		{
			name: "doer quotes the authorization header",
			doer: doerFunc(func(_ context.Context, req Request) (Response, error) {
				return Response{}, fmt.Errorf("GET %s failed with %s", req.URL, req.Header["Authorization"])
			}),
		},
		{
			name: "body echoes the token on a bad status",
			doer: respondWith(400, echoBody),
		},
		{
			name: "body echoes the token and does not parse",
			doer: respondWith(200, append([]byte("not json "), echoBody...)),
		},
		{
			name: "body echoes the token and carries no window",
			doer: respondWith(200, echoBody),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(tc.doer, time.Second)
			snap, err := client.Fetch(context.Background(), "auth-4", "3", testToken)
			if err == nil {
				t.Fatal("Fetch succeeded, want a failure")
			}
			assertNoToken(t, err.Error())
			assertNoToken(t, snap.Err)
			if strings.Contains(err.Error(), "is not permitted") {
				t.Errorf("error quotes the response body: %q", err.Error())
			}
		})
	}
}

func TestClientFetchRetainsNothingFromTheBody(t *testing.T) {
	client := newTestClient(respondWith(500, []byte("internal detail: user@example.com")), time.Second)
	_, err := client.Fetch(context.Background(), "auth-5", "4", testToken)
	if err == nil {
		t.Fatal("Fetch succeeded, want a failure")
	}
	if strings.Contains(err.Error(), "user@example.com") {
		t.Errorf("error quotes the response body: %q", err.Error())
	}
}

func TestNewClientDefaultsToTheAnthropicEndpoint(t *testing.T) {
	client := NewClient(nil, "", 0)
	if client.url != model.DefaultUsageURL {
		t.Errorf("url = %q, want %q", client.url, model.DefaultUsageURL)
	}
	if client.now == nil {
		t.Error("now is nil")
	}
}

func assertNoToken(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, testToken) {
		t.Errorf("string leaks the access token: %q", s)
	}
	if strings.Contains(s, "Bearer ") {
		t.Errorf("string leaks an authorization header: %q", s)
	}
}

// throttleThenServe answers the first n calls with 429 and the rest with a
// usable body, counting every call.
func throttleThenServe(t *testing.T, n int, retryAfter string) (*int, doerFunc) {
	t.Helper()
	body := fixture(t, "usage_late_week.json")
	calls := 0
	return &calls, func(context.Context, Request) (Response, error) {
		calls++
		if calls <= n {
			resp := Response{StatusCode: 429}
			if retryAfter != "" {
				resp.Header = map[string][]string{"Retry-After": {retryAfter}}
			}
			return resp, nil
		}
		return Response{StatusCode: 200, Body: body}, nil
	}
}

// A throttle costs the pool every reading at once, so one retry inside the
// same fetch is worth its wall clock.
func TestThrottleIsRetriedOnce(t *testing.T) {
	calls, doer := throttleThenServe(t, 1, "")
	client := newTestClient(doer, time.Second)

	snap, err := client.Fetch(context.Background(), "auth-1", "0", testToken)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want the request and one retry", *calls)
	}
	if len(snap.Windows) == 0 {
		t.Error("the retry's windows did not reach the snapshot")
	}

	// A throttle that outlasts the retry is reported, and only once retried.
	calls, doer = throttleThenServe(t, 9, "")
	client = newTestClient(doer, time.Second)
	if _, err = client.Fetch(context.Background(), "auth-1", "0", testToken); Category(err) != CategoryRateLimited {
		t.Errorf("category = %q, want %q", Category(err), CategoryRateLimited)
	}
	if *calls != 2 {
		t.Errorf("calls = %d, want no more than one retry", *calls)
	}
}

// Retry-After is the endpoint saying when it will answer. A hint past the cap
// is longer than the poll interval it would block, so the next poll takes it.
func TestRetryAfterIsHonouredUpToTheCap(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		wantCalls int
	}{
		{"no hint uses the default backoff", "", 2},
		{"a short hint is waited out", "1", 2},
		{"a hint past the cap skips the retry", "600", 1},
		{"an unreadable hint falls back to the backoff", "Wed, 09 Sep 2026 10:00:00 GMT", 2},
		{"a negative hint falls back to the backoff", "-5", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, doer := throttleThenServe(t, 1, tc.header)
			client := newTestClient(doer, 2*time.Second)
			_, _ = client.Fetch(context.Background(), "auth-1", "0", testToken)
			if *calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", *calls, tc.wantCalls)
			}
		})
	}
}

// Only a throttle is retried: every other failure means the same thing twice.
func TestOtherFailuresAreNotRetried(t *testing.T) {
	cases := []struct {
		name string
		doer doerFunc
	}{
		{"unauthorized", respondWith(401, nil)},
		{"forbidden", respondWith(403, nil)},
		{"server error", respondWith(500, nil)},
		{"unparseable body", respondWith(200, []byte("<html>nope</html>"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			counted := doerFunc(func(ctx context.Context, req Request) (Response, error) {
				calls++
				return tc.doer(ctx, req)
			})
			client := newTestClient(counted, time.Second)
			if _, err := client.Fetch(context.Background(), "auth-1", "0", testToken); err == nil {
				t.Fatal("Fetch reported success")
			}
			if calls != 1 {
				t.Errorf("calls = %d, want exactly one", calls)
			}
		})
	}
}

// A context that dies during the backoff ends the wait rather than holding the
// poll for it, so a stop mid-throttle is not paid for twice.
func TestARetryDoesNotOutlastTheContext(t *testing.T) {
	calls := 0
	var cancel context.CancelFunc
	client := newTestClient(doerFunc(func(context.Context, Request) (Response, error) {
		calls++
		// The throttle lands, then the context dies while the backoff runs.
		cancel()
		return Response{StatusCode: 429}, nil
	}), time.Second)
	ctx, stop := context.WithCancel(context.Background())
	cancel = stop
	defer stop()

	start := time.Now()
	if _, err := client.Fetch(ctx, "auth-1", "0", testToken); err == nil {
		t.Fatal("Fetch reported success")
	}
	if elapsed := time.Since(start); elapsed >= retryBackoff {
		t.Errorf("Fetch took %v, want it to give up rather than wait out %v", elapsed, retryBackoff)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want the retry skipped", calls)
	}
}
