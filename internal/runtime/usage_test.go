package runtime

import (
	"context"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func TestUsageHandleAccumulatesCacheStatsAndMergesHeaders(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.quota.Put(seatA(t))

	rec := UsageRecord{
		Provider: "claude", Model: fableModel, AuthID: "seat-a", AuthIndex: "idx-a",
		RequestedAt: testNow.Add(-time.Minute), Latency: 2 * time.Minute,
		Detail: UsageDetail{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 9000, CacheCreationTokens: 500},
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization":       {"0.42"},
			"Anthropic-Ratelimit-Unified-5h-Reset":             {"1757048400"},
			"Anthropic-Ratelimit-Unified-5h-Status":            {"allowed"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
			"Anthropic-Ratelimit-Unified-Status":               {"allowed"},
		},
	}
	tp.callOK(t, MethodUsageHandle, mustJSON(t, rec), nil)
	rec.Detail = UsageDetail{InputTokens: 50, OutputTokens: 10, CacheReadTokens: 1000}
	rec.ResponseHeaders = nil
	tp.callOK(t, MethodUsageHandle, mustJSON(t, rec), nil)

	stats := tp.cacheStats("seat-a")
	want := model.CacheStats{Requests: 2, CacheReadTokens: 10000, CacheCreationTokens: 500, FreshInputTokens: 150, OutputTokens: 50}
	if stats != want {
		t.Errorf("cache stats = %+v, want %+v", stats, want)
	}

	snap, ok := tp.quota.Get("seat-a")
	if !ok {
		t.Fatal("snapshot vanished")
	}
	session, ok := snap.Window(model.WindowSession, "")
	if !ok || session.Utilization != 0.42 || session.Status != model.StatusAllowed || !session.Active {
		t.Errorf("session window = %+v ok=%v, want the header reading merged in", session, ok)
	}
	if weekly, ok := snap.Window(model.WindowWeekly, ""); !ok || weekly.Utilization != 0.04 {
		t.Errorf("weekly window = %+v ok=%v, want the endpoint reading untouched", weekly, ok)
	}
	// With no first byte measured, the reading is stamped at the end of the
	// request, RequestedAt+Latency.
	if !snap.ObservedAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("ObservedAt = %v, want the response time", snap.ObservedAt)
	}
}

// TestALongStreamsHeadersLoseToALaterRequests covers two requests on one seat
// in a fast climb: a long stream admitted at 58% ends after a short request
// admitted later read 73%. The stream's headers are stamped at its first
// byte, so they are the older reading and leave the window at 73%.
func TestALongStreamsHeadersLoseToALaterRequests(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.quota.Put(seatA(t))
	record := func(requested time.Time, ttft, latency time.Duration, utilization string) UsageRecord {
		return UsageRecord{
			Provider: "claude", Model: fableModel, AuthID: "seat-a", AuthIndex: "idx-a",
			RequestedAt: requested, TTFT: ttft, Latency: latency,
			ResponseHeaders: http.Header{
				"Anthropic-Ratelimit-Unified-5h-Utilization": {utilization},
				"Anthropic-Ratelimit-Unified-5h-Reset":       {"1757048400"},
			},
		}
	}
	tp.callOK(t, MethodUsageHandle, mustJSON(t, record(testNow.Add(3*time.Minute), 30*time.Second, 40*time.Second, "0.73")), nil)
	tp.callOK(t, MethodUsageHandle, mustJSON(t, record(testNow.Add(time.Minute), 2*time.Second, 5*time.Minute, "0.58")), nil)

	snap, _ := tp.quota.Get("seat-a")
	if session, ok := snap.Window(model.WindowSession, ""); !ok || session.Utilization != 0.73 {
		t.Errorf("session window = %+v ok=%v, want the later request's 0.73 kept", session, ok)
	}
	if want := testNow.Add(210 * time.Second); !snap.ObservedAt.Equal(want) {
		t.Errorf("ObservedAt = %v, want the later request's first byte %v", snap.ObservedAt, want)
	}
}

// TestUsageCountersAloneOpenNoStatusRow pins what a usage record on its own is
// worth to the status view: an id and counters, with no label, provider,
// priority or status to put beside them.
func TestUsageCountersAloneOpenNoStatusRow(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	pollFixture(t, tp)
	if err := tp.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	listed := len(tp.Status(testNow, "").Auths)

	tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
		Provider: "claude", Model: fableModel, AuthID: "not-a-listed-credential",
		Detail: UsageDetail{InputTokens: 10},
	}), nil)
	if got := tp.cacheStats("not-a-listed-credential").Requests; got != 1 {
		t.Fatalf("cache requests = %d, want the record counted", got)
	}
	if got := len(tp.Status(testNow, "").Auths); got != listed {
		t.Errorf("status rows = %d, want the %d the host listed", got, listed)
	}
}

func TestUsageHandleNeverErrors(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	for _, payload := range [][]byte{nil, []byte("garbage"), []byte(`{"AuthID":""}`)} {
		var out map[string]any
		tp.callOK(t, MethodUsageHandle, payload, &out)
		if len(out) != 0 {
			t.Errorf("usage.handle(%q) = %v, want {}", payload, out)
		}
	}
	if len(tp.quota.All()) != 0 || len(tp.cache) != 0 {
		t.Error("an invalid record left state behind")
	}
}

// TestUsageRecordsARefusalSpan covers a Fable cap refusal end to end: the
// 429's headers open a span on the cap, a served Sonnet request leaves it
// running, and a served Fable request ends it at its first byte. The status
// view publishes the span with the cap's history.
func TestUsageRecordsARefusalSpan(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.quota.Put(seatA(t))
	reset := at(t, "2026-09-11T18:59:59Z")
	epoch := strconv.FormatInt(reset.Unix(), 10)

	refused := testNow.Add(time.Minute)
	tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
		Provider: "claude", Model: fableModel, AuthID: "seat-a", AuthIndex: "idx-a",
		RequestedAt: testNow, Latency: time.Minute,
		Failed: true, Failure: UsageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization":       {"0.6"},
			"Anthropic-Ratelimit-Unified-7d-Reset":             {epoch},
			"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization":    {"1.0"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":          {epoch},
			"Anthropic-Ratelimit-Unified-7d_oi-Status":         {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
		},
	}), nil)
	served := func(modelID string, requested time.Time) {
		tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
			Provider: "claude", Model: modelID, AuthID: "seat-a", AuthIndex: "idx-a",
			RequestedAt: requested, TTFT: 2 * time.Second, Latency: time.Minute,
		}), nil)
	}
	fableLocks := func() []model.Lock {
		t.Helper()
		for _, row := range tp.Status(testNow, "").Auths {
			if row.AuthID != "seat-a" {
				continue
			}
			for _, h := range row.History {
				if h.Kind == model.WindowWeeklyScoped && h.Scope == model.FamilyFable {
					return h.Locks
				}
			}
		}
		t.Fatal("no Fable cap history for seat-a")
		return nil
	}

	served("claude-sonnet-4-5", testNow.Add(10*time.Minute))
	if got, want := fableLocks(), []model.Lock{{From: refused.Unix(), To: reset.Unix()}}; !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a served Sonnet request = %v, want the span running to the reset %v", got, want)
	}
	served(fableModel, testNow.Add(20*time.Minute))
	end := testNow.Add(20*time.Minute + 2*time.Second)
	if got, want := fableLocks(), []model.Lock{{From: refused.Unix(), To: end.Unix(), End: model.LockEndServed}}; !reflect.DeepEqual(got, want) {
		t.Errorf("locks after a served Fable request = %v, want %v", got, want)
	}
}

// windowLocks reports the refusal spans seat-a publishes for one window.
func windowLocks(t *testing.T, tp *testPlugin, kind model.WindowKind, scope string) []model.Lock {
	t.Helper()
	for _, row := range tp.Status(testNow, "").Auths {
		if row.AuthID != "seat-a" {
			continue
		}
		for _, h := range row.History {
			if h.Kind == kind && h.Scope == scope {
				return h.Locks
			}
		}
	}
	return nil
}

// sessionRecord is a usage record for seat-a whose headers report the 5-hour
// window at utilization util with status, resetting at reset.
func sessionRecord(modelID string, requested time.Time, ttft time.Duration, reset time.Time, util, status string, failed bool) UsageRecord {
	epoch := strconv.FormatInt(reset.Unix(), 10)
	r := UsageRecord{
		Provider: "claude", Model: modelID, AuthID: "seat-a", AuthIndex: "idx-a",
		RequestedAt: requested, TTFT: ttft, Latency: ttft + time.Minute, Failed: failed,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization":       {util},
			"Anthropic-Ratelimit-Unified-5h-Reset":             {epoch},
			"Anthropic-Ratelimit-Unified-5h-Status":            {status},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
			"Anthropic-Ratelimit-Unified-Status":               {status},
		},
	}
	if failed {
		r.Latency = ttft
		r.Failure = UsageFailure{StatusCode: http.StatusTooManyRequests}
	}
	return r
}

// TestUsageRefusalSpanAgainstInFlightSuccess covers a 5-hour refusal and a
// success whose records land in either order: only a success admitted after
// the refusal ends its span, wherever its first byte falls.
func TestUsageRefusalSpanAgainstInFlightSuccess(t *testing.T) {
	reset := at(t, "2026-09-05T03:09:59Z")
	refusedAt := testNow.Add(10 * time.Minute)
	refusal := sessionRecord("claude-opus-4-5", refusedAt.Add(-time.Second), time.Second, reset, "1.0", "rejected", true)
	success := func(requested time.Time, ttft time.Duration) UsageRecord {
		return sessionRecord("claude-opus-4-5", requested, ttft, reset, "0.99", "allowed_warning", false)
	}
	running := []model.Lock{{From: refusedAt.Unix(), To: reset.Unix()}}
	for _, tc := range []struct {
		name string
		recs []UsageRecord
		want []model.Lock
	}{
		{"a success admitted before the refusal, first byte after it", []UsageRecord{refusal, success(refusedAt.Add(-20*time.Second), 25*time.Second)}, running},
		{"a success admitted before the refusal, first byte in its second", []UsageRecord{
			sessionRecord("claude-opus-4-5", refusedAt.Add(-time.Second), 1100*time.Millisecond, reset, "1.0", "rejected", true),
			success(refusedAt.Add(-20*time.Second), 20700*time.Millisecond),
		}, running},
		{"a success handled before the refusal", []UsageRecord{success(refusedAt.Add(-20*time.Second), 21*time.Second), refusal}, running},
		{"a success admitted after the refusal", []UsageRecord{refusal, success(refusedAt.Add(time.Minute), 2*time.Second)}, []model.Lock{{From: refusedAt.Unix(), To: refusedAt.Add(time.Minute + 2*time.Second).Unix(), End: model.LockEndServed}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tp := newTestPlugin(t, testConfigYAML)
			tp.quota.Put(seatA(t))
			for _, rec := range tc.recs {
				tp.callOK(t, MethodUsageHandle, mustJSON(t, rec), nil)
			}
			if got := windowLocks(t, tp, model.WindowSession, ""); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("5-hour locks = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUsageServedResponseRefusesNoBearingWindow covers an overage response: a
// served request whose headers report a window bearing on its model as
// rejected opens no span on it, while a rejected cap on another family does.
func TestUsageServedResponseRefusesNoBearingWindow(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.quota.Put(seatA(t))
	reset := at(t, "2026-09-05T03:09:59Z")
	tp.callOK(t, MethodUsageHandle, mustJSON(t, sessionRecord("claude-sonnet-4-5", testNow, time.Second, reset, "1.0", "rejected", false)), nil)
	if got := windowLocks(t, tp, model.WindowSession, ""); got != nil {
		t.Errorf("5-hour locks after a served overage response = %v, want none", got)
	}

	weeklyReset := at(t, "2026-09-11T18:59:59Z")
	epoch := strconv.FormatInt(weeklyReset.Unix(), 10)
	requested := testNow.Add(time.Minute)
	tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
		Provider: "claude", Model: "claude-sonnet-4-5", AuthID: "seat-a", AuthIndex: "idx-a",
		RequestedAt: requested, TTFT: time.Second, Latency: time.Minute,
		ResponseHeaders: http.Header{
			"Anthropic-Ratelimit-Unified-7d-Utilization":       {"0.6"},
			"Anthropic-Ratelimit-Unified-7d-Reset":             {epoch},
			"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization":    {"1.0"},
			"Anthropic-Ratelimit-Unified-7d_oi-Reset":          {epoch},
			"Anthropic-Ratelimit-Unified-7d_oi-Status":         {"rejected"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
			"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
		},
	}), nil)
	opened := requested.Add(time.Second)
	if got, want := windowLocks(t, tp, model.WindowWeeklyScoped, model.FamilyFable), []model.Lock{{From: opened.Unix(), To: weeklyReset.Unix()}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Fable locks after a served Sonnet request = %v, want %v", got, want)
	}
	tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
		Provider: "claude", Model: fableModel, AuthID: "seat-a", AuthIndex: "idx-a",
		RequestedAt: requested.Add(time.Minute), TTFT: time.Second, Latency: time.Minute,
	}), nil)
	ended := requested.Add(time.Minute + time.Second)
	if got, want := windowLocks(t, tp, model.WindowWeeklyScoped, model.FamilyFable), []model.Lock{{From: opened.Unix(), To: ended.Unix(), End: model.LockEndServed}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Fable locks after a served Fable request = %v, want %v", got, want)
	}
}

// fableCapHeaders is a response's headers with the 7-day window allowed and
// the Fable cap at status, both resetting at reset.
func fableCapHeaders(reset time.Time, status string) http.Header {
	epoch := strconv.FormatInt(reset.Unix(), 10)
	return http.Header{
		"Anthropic-Ratelimit-Unified-7d-Utilization":       {"0.6"},
		"Anthropic-Ratelimit-Unified-7d-Reset":             {epoch},
		"Anthropic-Ratelimit-Unified-7d-Status":            {"allowed"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization":    {"1.0"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":          {epoch},
		"Anthropic-Ratelimit-Unified-7d_oi-Status":         {status},
		"Anthropic-Ratelimit-Unified-Representative-Claim": {"seven_day_overage_included"},
		"Anthropic-Ratelimit-Unified-Status":               {status},
	}
}

// TestUsageLateRefusalAfterAServedRequest covers a refusal whose record lands
// after that of a request admitted later and served: a slow 429, or another
// family's stream whose headers report the cap rejected, neither reopens the
// span the served request ended nor opens a new one.
func TestUsageLateRefusalAfterAServedRequest(t *testing.T) {
	t.Run("a slow 429 on the 5-hour window", func(t *testing.T) {
		tp := newTestPlugin(t, testConfigYAML)
		tp.quota.Put(seatA(t))
		reset := at(t, "2026-09-05T03:09:59Z")
		tp.callOK(t, MethodUsageHandle, mustJSON(t, sessionRecord("claude-opus-4-5", testNow, time.Second, reset, "1.0", "rejected", true)), nil)
		admitted := testNow.Add(30*time.Minute + 500*time.Millisecond)
		tp.callOK(t, MethodUsageHandle, mustJSON(t, sessionRecord("claude-opus-4-5", admitted, time.Second, reset, "0.2", "allowed", false)), nil)
		tp.callOK(t, MethodUsageHandle, mustJSON(t, sessionRecord("claude-opus-4-5", testNow.Add(30*time.Minute), 3*time.Second, reset, "1.0", "rejected", true)), nil)
		want := []model.Lock{{From: testNow.Add(time.Second).Unix(), To: admitted.Add(time.Second).Unix(), End: model.LockEndServed}}
		if got := windowLocks(t, tp, model.WindowSession, ""); !reflect.DeepEqual(got, want) {
			t.Errorf("5-hour locks = %v, want the span the served request ended %v", got, want)
		}
	})
	t.Run("a Sonnet stream reporting the Fable cap rejected", func(t *testing.T) {
		tp := newTestPlugin(t, testConfigYAML)
		tp.quota.Put(seatA(t))
		reset := at(t, "2026-09-11T18:59:59Z")
		tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
			Provider: "claude", Model: fableModel, AuthID: "seat-a", AuthIndex: "idx-a",
			RequestedAt: testNow, TTFT: time.Second, Latency: time.Second,
			Failed: true, Failure: UsageFailure{StatusCode: http.StatusTooManyRequests}, ResponseHeaders: fableCapHeaders(reset, "rejected"),
		}), nil)
		tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
			Provider: "claude", Model: fableModel, AuthID: "seat-a", AuthIndex: "idx-a",
			RequestedAt: testNow.Add(31 * time.Minute), TTFT: time.Second, Latency: 5 * time.Second,
			ResponseHeaders: fableCapHeaders(reset, "allowed"),
		}), nil)
		tp.callOK(t, MethodUsageHandle, mustJSON(t, UsageRecord{
			Provider: "claude", Model: "claude-sonnet-4-5", AuthID: "seat-a", AuthIndex: "idx-a",
			RequestedAt: testNow.Add(30 * time.Minute), TTFT: time.Second, Latency: 6 * time.Minute,
			ResponseHeaders: fableCapHeaders(reset, "rejected"),
		}), nil)
		want := []model.Lock{{From: testNow.Add(time.Second).Unix(), To: testNow.Add(31*time.Minute + time.Second).Unix(), End: model.LockEndServed}}
		if got := windowLocks(t, tp, model.WindowWeeklyScoped, model.FamilyFable); !reflect.DeepEqual(got, want) {
			t.Errorf("Fable locks = %v, want the span the served Fable request ended %v", got, want)
		}
	})
}

// TestUsageFailureOtherThanA429RefusesNoBearingWindow covers a served stream
// that fails mid-stream: its record carries the 200's headers, and a window
// bearing on its model that they report rejected opens no span.
func TestUsageFailureOtherThanA429RefusesNoBearingWindow(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.quota.Put(seatA(t))
	rec := sessionRecord("claude-sonnet-4-5", testNow, 2*time.Second, at(t, "2026-09-05T03:09:59Z"), "1.0", "rejected", true)
	rec.Failure = UsageFailure{StatusCode: http.StatusOK}
	tp.callOK(t, MethodUsageHandle, mustJSON(t, rec), nil)
	if got := windowLocks(t, tp, model.WindowSession, ""); got != nil {
		t.Errorf("5-hour locks after a served stream that failed = %v, want none", got)
	}
}
