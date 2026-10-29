package runtime

import (
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/quota"
)

// spendSpan is how far back a seat's use is set against what it served;
// spendKeep is how long an hour's spend is kept.
const (
	spendSpan = 24 * time.Hour
	spendKeep = spendSpan + time.Hour
)

// A seat's rate is reported only once the span holds this much spend and this
// much rise for each window cycle the rise spans. A reading is a whole
// percent, so each cycle's rise is known to within a percentage point, a fifth
// of what it must add.
const (
	minRateUSD  = 0.5
	minRateRise = 0.05
)

// listPrice is a model's API list price in US dollars per million tokens. A
// cache write is priced as a one-hour write, twice input: the host reports no
// write's lifetime, so a five-minute write, at 1.25 times input, counts high.
type listPrice struct{ input, output, cacheRead float64 }

// listPrices are matched in order against a model id's lowercase form, so a
// specific version comes before its family; an id matching none takes the
// last. The table weighs one model's tokens against another's, the same
// weighing Claude Code's own session cost applies.
var listPrices = []struct {
	match string
	price listPrice
}{
	{"fable-5-1", listPrice{10, 50, 0.25}},
	{"mythos-5-1", listPrice{10, 50, 0.25}},
	{"fable", listPrice{10, 50, 1}},
	{"mythos", listPrice{10, 50, 1}},
	{"haiku-5", listPrice{0.10, 0.50, 0.01}},
	{"haiku", listPrice{1, 5, 0.10}},
	{"sonnet-5", listPrice{2, 10, 0.20}},
	{"sonnet", listPrice{3, 15, 0.30}},
	{"opus-5-5", listPrice{4, 20, 0.20}},
	{"opus", listPrice{5, 25, 0.50}},
	{"", listPrice{4, 20, 0.20}},
}

// costOf is what a request's tokens cost at the model's list price.
func costOf(modelID string, d UsageDetail) float64 {
	lower := strings.ToLower(modelID)
	var p listPrice
	for _, lp := range listPrices {
		if strings.Contains(lower, lp.match) {
			p = lp.price
			break
		}
	}
	return (float64(d.InputTokens)*p.input +
		float64(d.CacheCreationTokens)*p.input*2 +
		float64(d.CacheReadTokens)*p.cacheRead +
		float64(d.OutputTokens)*p.output) / 1e6
}

// spendLedger is one seat's spend at list price, in hourly buckets keyed by
// the hour's start in unix seconds.
type spendLedger map[int64]float64

func spendHour(t time.Time) int64 { return t.Truncate(time.Hour).Unix() }

// addSpendLocked records a request's cost against its seat, dropping the hours
// past spendKeep and any after the current hour, which a clock set back
// leaves. The caller holds p.mu.
func (p *Plugin) addSpendLocked(authID, modelID string, d UsageDetail, at time.Time) {
	cost := costOf(modelID, d)
	if cost <= 0 {
		return
	}
	led := p.spend[authID]
	if led == nil {
		led = spendLedger{}
		p.spend[authID] = led
	}
	led[spendHour(at)] += cost
	now := p.now()
	cut, last := spendHour(now.Add(-spendKeep)), spendHour(now)
	for h := range led {
		if h < cut || h > last {
			delete(led, h)
		}
	}
}

// SpendRates gives each seat the plugin has seen spend on its rates, by
// credential id, from the same history the status publishes. A seat with no
// rate in either window is left out.
func (p *Plugin) SpendRates(now time.Time) map[string]model.SpendRates {
	p.mu.Lock()
	ids := make([]string, 0, len(p.spend))
	for id := range p.spend {
		ids = append(ids, id)
	}
	p.mu.Unlock()
	rates := make(map[string]model.SpendRates, len(ids))
	for _, id := range ids {
		history := p.quota.History(id, quota.HistoryPublishMax, now)
		r := model.SpendRates{
			Weekly:   p.perUSD(id, history, model.WindowWeekly, now),
			FiveHour: p.perUSD(id, history, model.WindowSession, now),
		}
		if r.Weekly != nil || r.FiveHour != nil {
			rates[id] = r
		}
	}
	return rates
}

// perUSD is how much of a seat's 7-day or 5-hour window one dollar of
// list-price spend used over the last spendSpan, from the window's rise and
// the spend the plugin saw in the same hours. Absent until the span holds
// enough of both. Spend is counted only while the plugin runs, so the span
// starts no earlier than the plugin did, and then the rise counts from the
// first reading the plugin took: use while it was not running lands in that
// reading.
func (p *Plugin) perUSD(authID string, history []model.WindowHistory, kind model.WindowKind, now time.Time) *float64 {
	p.mu.Lock()
	since := time.Unix(spendHour(now.Add(-spendSpan)), 0)
	started := p.startedAt.After(since)
	if started {
		since = p.startedAt
	}
	usd := 0.0
	for h, c := range p.spend[authID] {
		if h >= spendHour(since) && h <= spendHour(now) {
			usd += c
		}
	}
	p.mu.Unlock()
	if usd < minRateUSD {
		return nil
	}
	length := model.WeeklyDuration
	if kind == model.WindowSession {
		length = model.SessionDuration
	}
	for _, h := range history {
		if h.Kind != kind || h.Scope != "" {
			continue
		}
		from := since
		first, cycles := readingsFrom(h, since)
		if started {
			if cycles == 0 {
				return nil
			}
			from = first
		}
		rise := h.Rise(from, length)
		if rise < minRateRise*float64(max(1, cycles)) {
			return nil
		}
		rate := rise / usd
		return &rate
	}
	return nil
}

// readingsFrom is when the history's first reading at or after t was taken,
// and how many of its cycles hold one, each a rise of its own.
func readingsFrom(h model.WindowHistory, t time.Time) (first time.Time, cycles int) {
	for _, c := range h.Cycles {
		for _, s := range c.Samples {
			if !s.At.Before(t) && s.Utilization >= 0 {
				if cycles == 0 {
					first = s.At
				}
				cycles++
				break
			}
		}
	}
	return first, cycles
}
