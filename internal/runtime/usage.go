package runtime

import (
	"encoding/json"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/quota"
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
	// The headers describe the response, so the reading is stamped at the end
	// of the request: stamping it at the start would lose a long request's
	// readings to a shorter later one and age every snapshot by the request's
	// own duration.
	observed := p.now()
	if !rec.RequestedAt.IsZero() {
		observed = rec.RequestedAt.Add(rec.Latency)
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
