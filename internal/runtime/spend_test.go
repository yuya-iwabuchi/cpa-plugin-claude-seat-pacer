package runtime

import (
	"math"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func TestCostOfWeighsEachModelAtItsListPrice(t *testing.T) {
	d := UsageDetail{InputTokens: 1e6, CacheCreationTokens: 1e6, CacheReadTokens: 1e6, OutputTokens: 1e6}
	for _, c := range []struct {
		model string
		want  float64
	}{
		{"claude-opus-5-5", 4 + 8 + 0.20 + 20},
		{"claude-fable-5-1", 10 + 20 + 0.25 + 50},
		{"claude-fable-5", 10 + 20 + 1 + 50},
		{"claude-haiku-5-5", 5 * (0.10 + 0.20 + 0.01 + 0.50)},
		{"claude-3-5-haiku-20241022", 0.80 + 1.60 + 0.08 + 4},
		{"claude-haiku-4-5", 1 + 2 + 0.10 + 5},
		{"claude-sonnet-5-5", 2 + 4 + 0.10 + 10},
		{"claude-sonnet-5", 2 + 4 + 0.20 + 10},
		{"claude-sonnet-4-6", 3 + 6 + 0.30 + 15},
		{"claude-opus-4-8", 5 + 10 + 0.50 + 25},
		{"claude-opus-4-1-20250805", 15 + 30 + 1.50 + 75},
		{"claude-opus-4-20250514", 15 + 30 + 1.50 + 75},
		{"some-new-model", 4 + 8 + 0.20 + 20},
	} {
		if got := costOf(c.model, d); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("costOf(%s) = %v, want %v", c.model, got, c.want)
		}
	}
	// A prompt of 100,000 tokens or fewer stays at the base price.
	short := UsageDetail{InputTokens: 40_000, CacheCreationTokens: 30_000, CacheReadTokens: 30_000, OutputTokens: 1e6}
	if got, want := costOf("claude-haiku-5-5", short), (0.04*0.10+0.03*0.20+0.03*0.01)+0.50; math.Abs(got-want) > 1e-9 {
		t.Errorf("costOf(haiku 5.5, 100k prompt) = %v, want %v", got, want)
	}
}

func TestSpendStartsOverAtEachRegister(t *testing.T) {
	p := newTestPlugin(t, "")
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-2 * time.Hour), Detail: UsageDetail{OutputTokens: 100_000}})
	// A rollback reconfigures the quiesced build, which kept its ledger.
	p.dispatch(MethodPluginQuiesce, nil)
	p.register(t, MethodPluginReconfigure, "")
	p.mu.Lock()
	kept, from := len(p.spend), p.spendFrom
	p.mu.Unlock()
	if kept != 1 || !from.Equal(testNow) {
		t.Errorf("after a rollback spend has %d seats from %v, want 1 from %v", kept, from, testNow)
	}
	p.dispatch(MethodPluginQuiesce, nil)
	later := testNow.Add(3 * time.Hour)
	p.now = func() time.Time { return later }
	p.register(t, MethodPluginRegister, "")
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.spend) != 0 || !p.spendFrom.Equal(later) {
		t.Errorf("after a register spend = %v from %v, want none from %v", p.spend, p.spendFrom, later)
	}
}

// weeklyHistory is one seat's 7-day window rising from 0.20 to 0.26 over the
// day before testNow.
func weeklyHistory() []model.WindowHistory {
	return []model.WindowHistory{{Kind: model.WindowWeekly, Cycles: []model.Cycle{{
		ResetsAt: testNow.Add(72 * time.Hour),
		Samples: []model.Sample{
			{At: testNow.Add(-30 * time.Hour), Utilization: 0.20},
			{At: testNow.Add(-time.Minute), Utilization: 0.26},
		},
	}}}}
}

func TestSpendRatesNameEachSeatWithARate(t *testing.T) {
	p := newTestPlugin(t, "")
	p.mu.Lock()
	p.spendFrom = testNow.Add(-48 * time.Hour)
	p.mu.Unlock()
	for _, id := range []string{"a", "b"} {
		p.quota.Put(model.AuthSnapshot{AuthID: id})
	}
	p.quota.ImportHistory(map[string][]model.WindowHistory{"a": weeklyHistory(), "b": weeklyHistory()}, testNow)
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-2 * time.Hour), Detail: UsageDetail{OutputTokens: 150_000}})
	p.usage(UsageRecord{AuthID: "c", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-2 * time.Hour), Detail: UsageDetail{OutputTokens: 150_000}})
	rates := p.SpendRates(testNow)
	if r, ok := rates["a"]; !ok || r.Weekly == nil || math.Abs(*r.Weekly-0.06/3) > 1e-9 || r.FiveHour != nil {
		t.Errorf("seat a = %+v, want the day's 0.06 over its $3 and no 5-hour rate", r)
	}
	if len(rates) != 1 {
		t.Errorf("rates = %v, want only seat a: b has no spend and c no history", rates)
	}
}

func TestWeeklyRateSetsTheDaysRiseAgainstItsSpend(t *testing.T) {
	p := newTestPlugin(t, "")
	p.mu.Lock()
	p.spendFrom = testNow.Add(-48 * time.Hour)
	p.mu.Unlock()
	// $3 of Opus 5.5 output inside the span, $40 a day before it, and $40 in
	// an hour still to come, as a clock set back leaves until the next request.
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-2 * time.Hour), Detail: UsageDetail{OutputTokens: 150_000}})
	p.mu.Lock()
	p.spend["a"][spendHour(testNow.Add(-26*time.Hour))] += 40
	p.spend["a"][spendHour(testNow.Add(2*time.Hour))] += 40
	p.mu.Unlock()
	rate := p.perUSD("a", weeklyHistory(), model.WindowWeekly, testNow)
	if rate == nil || math.Abs(*rate-0.06/3) > 1e-9 {
		t.Fatalf("rate = %v, want the day's 0.06 over its $3", rate)
	}
	if p.perUSD("b", weeklyHistory(), model.WindowWeekly, testNow) != nil {
		t.Error("a seat with no spend has a rate")
	}
}

// The 5-hour window's rate sums its rises across the cycles in the span.
func TestFiveHourRateSumsTheSpansCycles(t *testing.T) {
	p := newTestPlugin(t, "")
	p.mu.Lock()
	p.spendFrom = testNow.Add(-48 * time.Hour)
	p.mu.Unlock()
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-2 * time.Hour), Detail: UsageDetail{OutputTokens: 200_000}})
	h := append(weeklyHistory(), model.WindowHistory{Kind: model.WindowSession, Cycles: []model.Cycle{
		{ResetsAt: testNow.Add(-6 * time.Hour), Samples: []model.Sample{
			{At: testNow.Add(-10 * time.Hour), Utilization: 0.10},
			{At: testNow.Add(-7 * time.Hour), Utilization: 0.30},
		}},
		{ResetsAt: testNow.Add(time.Hour), Samples: []model.Sample{
			{At: testNow.Add(-3 * time.Hour), Utilization: 0.05},
			{At: testNow.Add(-time.Minute), Utilization: 0.25},
		}},
	}})
	rate := p.perUSD("a", h, model.WindowSession, testNow)
	if rate == nil || math.Abs(*rate-0.55/4) > 1e-9 {
		t.Fatalf("rate = %v, want the span's 0.55 over its $4", rate)
	}
	if week := p.perUSD("a", h, model.WindowWeekly, testNow); week == nil || math.Abs(*week-0.06/4) > 1e-9 {
		t.Errorf("weekly rate = %v, want 0.06 over $4", week)
	}
}

func TestWeeklyRateWaitsForEnoughSpendAndUse(t *testing.T) {
	p := newTestPlugin(t, "")
	p.mu.Lock()
	p.spendFrom = testNow.Add(-48 * time.Hour)
	p.mu.Unlock()
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-time.Hour), Detail: UsageDetail{OutputTokens: 10_000}})
	if p.perUSD("a", weeklyHistory(), model.WindowWeekly, testNow) != nil {
		t.Error("a rate from $0.20 of spend")
	}
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-time.Hour), Detail: UsageDetail{OutputTokens: 100_000}})
	flat := weeklyHistory()
	flat[0].Cycles[0].Samples[1].Utilization = 0.24
	if p.perUSD("a", flat, model.WindowWeekly, testNow) != nil {
		t.Error("a rate from a rise of a few whole-percent readings")
	}
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", Failed: true, RequestedAt: testNow, Detail: UsageDetail{OutputTokens: 50_000}})
	p.mu.Lock()
	defer p.mu.Unlock()
	if got := p.spend["a"][spendHour(testNow)]; math.Abs(got-1) > 1e-9 {
		t.Errorf("a request cut off after spending $1 counted %v", got)
	}
}

// Each cycle's rise carries its own rounding, so a rise spread over several
// 5-hour cycles needs more of it.
func TestFiveHourRateWaitsForARisePerCycle(t *testing.T) {
	p := newTestPlugin(t, "")
	p.mu.Lock()
	p.spendFrom = testNow.Add(-48 * time.Hour)
	p.mu.Unlock()
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-2 * time.Hour), Detail: UsageDetail{OutputTokens: 200_000}})
	var cycles []model.Cycle
	for i := range 5 {
		end := testNow.Add(time.Duration(-20+5*i) * time.Hour)
		cycles = append(cycles, model.Cycle{ResetsAt: end.Add(time.Hour), Samples: []model.Sample{
			{At: end.Add(-3 * time.Hour), Utilization: 0},
			{At: end, Utilization: 0.02},
		}})
	}
	h := []model.WindowHistory{{Kind: model.WindowSession, Cycles: cycles}}
	if rate := p.perUSD("a", h, model.WindowSession, testNow); rate != nil {
		t.Errorf("rate = %v from five cycles of 2%% each", *rate)
	}
	for i := range cycles {
		cycles[i].Samples[1].Utilization = 0.05
	}
	if rate := p.perUSD("a", h, model.WindowSession, testNow); rate == nil || math.Abs(*rate-0.25/4) > 1e-9 {
		t.Errorf("rate = %v, want five cycles' 0.25 over $4", rate)
	}
}

func TestWeeklyRateStartsNoEarlierThanTheLedger(t *testing.T) {
	p := newTestPlugin(t, "")
	p.mu.Lock()
	p.spendFrom = testNow.Add(-90 * time.Minute)
	p.mu.Unlock()
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-time.Hour), Detail: UsageDetail{OutputTokens: 100_000}})
	// The window rose 0.11 over the day. The plugin's first reading after its
	// ledger started holds the use made before it, so only the 0.06 after that
	// reading counts against the spend it saw.
	h := weeklyHistory()
	h[0].Cycles[0].Samples = append(h[0].Cycles[0].Samples[:1],
		model.Sample{At: testNow.Add(-100 * time.Minute), Utilization: 0.22},
		model.Sample{At: testNow.Add(-80 * time.Minute), Utilization: 0.25},
		model.Sample{At: testNow.Add(-time.Minute), Utilization: 0.31})
	rate := p.perUSD("a", h, model.WindowWeekly, testNow)
	if rate == nil || math.Abs(*rate-0.06/2) > 1e-9 {
		t.Fatalf("rate = %v, want 0.06 over $2", rate)
	}
}

func TestSpendKeepsOnlyTheHoursTheRateReads(t *testing.T) {
	p := newTestPlugin(t, "")
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(-30 * time.Hour), Detail: UsageDetail{OutputTokens: 1000}})
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow, Detail: UsageDetail{OutputTokens: 1000}})
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.spend["a"]) != 1 {
		t.Errorf("spend hours = %v, want only the newest", p.spend["a"])
	}
	p.mu.Unlock()
	// An hour booked ahead of the clock, as a clock set back leaves one.
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow.Add(3 * time.Hour), Detail: UsageDetail{OutputTokens: 1000}})
	p.usage(UsageRecord{AuthID: "a", Model: "claude-opus-5-5", RequestedAt: testNow, Detail: UsageDetail{OutputTokens: 1000}})
	p.mu.Lock()
	if _, ok := p.spend["a"][spendHour(testNow.Add(3*time.Hour))]; ok {
		t.Error("an hour after the current one was kept")
	}
}
