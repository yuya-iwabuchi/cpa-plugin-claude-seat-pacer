package runtime

import (
	"encoding/json"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// usageEnvelope answers usage.handle. The host expects an empty object back
// and only debug-logs a failure, so every outcome here is `{}`.
func (p *Plugin) usageEnvelope(payload []byte) ([]byte, error) {
	var rec UsageRecord
	if err := json.Unmarshal(payload, &rec); err == nil {
		p.usage(rec)
	}
	return okEnvelope(emptyResult)
}

// usage folds one completed request into the per-credential cache counters and
// the quota store. The rate-limit headers on a real response are the free
// quota signal: fresher than a usage-endpoint read, but covering only the
// windows this request touched, which is why they merge rather than replace.
func (p *Plugin) usage(rec UsageRecord) {
	if rec.AuthID == "" {
		return
	}
	p.mu.Lock()
	stats := p.cache[rec.AuthID]
	stats.Requests++
	stats.CacheReadTokens += rec.Detail.CacheReadTokens
	stats.CacheCreationTokens += rec.Detail.CacheCreationTokens
	stats.OutputTokens += rec.Detail.OutputTokens
	// InputTokens is the provider's uncached input count, which the split
	// cache counters do not include.
	stats.FreshInputTokens += rec.Detail.InputTokens
	p.cache[rec.AuthID] = stats
	p.mu.Unlock()

	if len(rec.ResponseHeaders) == 0 {
		return
	}
	// The headers arrive ahead of the first body byte and describe the
	// window as the provider admitted the request, so the reading is stamped
	// at the first byte. A streamed response can run for minutes past it, and
	// MergeHeaders drops a reading older than the one it holds, so a long
	// request's reading loses to a later request's. TTFT is zero where the host
	// measured no first byte, and the end of the request stands in.
	observed := p.now()
	if !rec.RequestedAt.IsZero() {
		first := rec.TTFT
		if first <= 0 {
			first = rec.Latency
		}
		observed = rec.RequestedAt.Add(first)
	}
	if windows := quota.ParseResponseHeaders(rec.ResponseHeaders, observed); len(windows) > 0 {
		p.quota.MergeHeaders(rec.AuthID, windows, observed)
	}
}

// cacheStats returns a copy of one credential's counters.
func (p *Plugin) cacheStats(authID string) model.CacheStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cache[authID]
}
