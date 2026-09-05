package runtime

import (
	"net/http"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

func TestUsageHandleAccumulatesCacheStatsAndMergesHeaders(t *testing.T) {
	tp := newTestPlugin(t, testConfigYAML)
	tp.quota.Put(seatA(t))

	rec := UsageRecord{
		Provider: "claude", Model: fableModel, AuthID: "seat-a", AuthIndex: "idx-a",
		RequestedAt: testNow.Add(time.Minute),
		Detail:      UsageDetail{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 9000, CacheCreationTokens: 500},
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
	if !snap.ObservedAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("ObservedAt = %v, want advanced to the response time", snap.ObservedAt)
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
