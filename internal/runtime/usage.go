package runtime

import (
	"encoding/json"
	"net/http"
	"slices"

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
//
// A window the headers report as rejected records a refusal. On a 429 every
// such window counts. On any other response a window bearing on the model
// counts for nothing: a served one admitted the request whatever its status
// says, which is what an overage response looks like, and a failure that is
// no 429 was not a refusal of it; a rejected cap on another family still
// refused. A served request then ends the refusal of every window bearing on
// its model that refused before the request was admitted.
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

	// The headers arrive ahead of the first body byte and describe the
	// window as the provider admitted the request, so the reading is stamped
	// at the first byte. A streamed response can run for minutes past it, and
	// MergeHeaders drops a reading older than the one it holds, so a long
	// request's reading loses to a later request's. TTFT is zero where the host
	// measured no first byte, and the end of the request stands in.
	observed, admitted := p.now(), rec.RequestedAt
	if !admitted.IsZero() {
		first := rec.TTFT
		if first <= 0 {
			first = rec.Latency
		}
		observed = admitted.Add(first)
	} else {
		admitted = observed
	}
	var windows []model.Window
	if len(rec.ResponseHeaders) > 0 {
		windows = quota.ParseResponseHeaders(rec.ResponseHeaders, observed)
		p.quota.MergeHeaders(rec.AuthID, windows, observed)
	}
	if rec.Failed && rec.Failure.StatusCode == http.StatusTooManyRequests {
		p.quota.RecordRefusals(rec.AuthID, windows, admitted, observed)
		return
	}
	family := model.FamilyOf(rec.Model)
	p.quota.RecordRefusals(rec.AuthID, slices.DeleteFunc(windows, func(w model.Window) bool { return w.BearsOn(family) }), admitted, observed)
	if !rec.Failed {
		p.quota.MarkServed(rec.AuthID, family, admitted, observed)
	}
}

// cacheStats returns a copy of one credential's counters.
func (p *Plugin) cacheStats(authID string) model.CacheStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cache[authID]
}
