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
// When longAbove is set, a request whose input, cache read and cache write
// tokens add up to more than longAbove costs longTimes as much.
type listPrice struct {
	input, output, cacheRead float64
	longAbove                int64
	longTimes                float64
}

// listPrices are matched in order against a model id's lowercase form, so a
// specific version comes before its family; an id matching none takes the
// last. They follow the prices in Claude Code's model catalog, which its
// session cost uses: a client multiplies that cost by a rate built from this
// table, so the two must weigh each model alike.
var listPrices = []struct {
	match string
	price listPrice
}{
	{"fable-5-20", listPrice{input: 10, output: 50, cacheRead: 1}},
	{"mythos-5-20", listPrice{input: 10, output: 50, cacheRead: 1}},
	{"fable-5-2", listPrice{input: 8, output: 40, cacheRead: 0.20}},
	{"mythos-5-2", listPrice{input: 8, output: 40, cacheRead: 0.20}},
	{"fable-5-1", listPrice{input: 10, output: 50, cacheRead: 0.25}},
	{"mythos-5-1", listPrice{input: 10, output: 50, cacheRead: 0.25}},
	{"fable", listPrice{input: 10, output: 50, cacheRead: 1}},
	{"mythos", listPrice{input: 10, output: 50, cacheRead: 1}},
	{"haiku-5", listPrice{input: 0.10, output: 0.50, cacheRead: 0.01, longAbove: 100_000, longTimes: 5}},
	{"3-5-haiku", listPrice{input: 0.80, output: 4, cacheRead: 0.08}},
	{"haiku", listPrice{input: 1, output: 5, cacheRead: 0.10}},
	{"sonnet-5-5", listPrice{input: 2, output: 10, cacheRead: 0.10}},
	{"sonnet-5", listPrice{input: 2, output: 10, cacheRead: 0.20}},
	{"sonnet", listPrice{input: 3, output: 15, cacheRead: 0.30}},
	{"opus-5-5", listPrice{input: 4, output: 20, cacheRead: 0.20}},
	{"opus-4-0", listPrice{input: 15, output: 75, cacheRead: 1.50}},
	{"opus-4-1", listPrice{input: 15, output: 75, cacheRead: 1.50}},
	{"opus-4-2025", listPrice{input: 15, output: 75, cacheRead: 1.50}},
	{"opus", listPrice{input: 5, output: 25, cacheRead: 0.50}},
	{"", listPrice{input: 4, output: 20, cacheRead: 0.20}},
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
	cost := (float64(d.InputTokens)*p.input +
		float64(d.CacheCreationTokens)*p.input*2 +
		float64(d.CacheReadTokens)*p.cacheRead +
		float64(d.OutputTokens)*p.output) / 1e6
	if p.longAbove > 0 && d.InputTokens+d.CacheReadTokens+d.CacheCreationTokens > p.longAbove {
		cost *= p.longTimes
	}
	return cost
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

// restartSpend empties the ledger and starts it now. configure calls it once
// per load: at plugin.register, or at the first configure of a build whose
// register the host failed. A load can follow a stretch in which
// another build, or none, served the seats and saw their spend. Its rollback of
// a failed update sends plugin.reconfigure to the build it kept, which went on
// serving, so that build keeps its ledger. A stretch the host spends without
// the plugin and ends with plugin.reconfigure, as after it fuses the plugin, is
// missed, and raises the rates until it leaves the span.
func (p *Plugin) restartSpend() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spendFrom = p.now()
	clear(p.spend)
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
// minRateUSD of spend and minRateRise of rise for each cycle. The ledger
// starts at spendFrom, so the span starts no earlier, and the rise then counts
// from the first reading at or after spendFrom: use before it lands in that
// reading.
func (p *Plugin) perUSD(authID string, history []model.WindowHistory, kind model.WindowKind, now time.Time) *float64 {
	p.mu.Lock()
	since := time.Unix(spendHour(now.Add(-spendSpan)), 0)
	started := p.spendFrom.After(since)
	if started {
		since = p.spendFrom
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
		rise := h.Rise(from)
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
			if !s.At.Before(t) && s.Usable() {
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
