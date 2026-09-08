package quota

import (
	"math"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// History retention. Samples newer than historyFineSpan keep one-minute
// spacing; older ones collapse to one per historyCoarseStep. At the fullest —
// a reading every minute for a day and one every ten minutes for the rest of a
// week — a window holds 2,304 samples, and historyMaxSamples leaves room for
// two more days beyond the week before the oldest are evicted. A sample is 32
// bytes in memory, so one window caps at 83 KB and a seat's three windows at
// 250 KB; a 2-minute poll with idle periods folded holds far fewer.
const (
	historyFineSpan   = 24 * time.Hour
	historyFineStep   = time.Minute
	historyCoarseStep = 10 * time.Minute
	historyMaxSamples = 2600
	// HistoryPublishMax bounds the samples a status response carries per
	// window: one per four pixels of a chart a thousand pixels wide, which
	// is finer than a 1.6px line shows.
	HistoryPublishMax = 240
)

// utilizationScale rounds a recorded utilization to a hundredth of a percent,
// which is finer than any chart draws and lets two idle readings compare
// equal.
const utilizationScale = 1e4

// ring is the recorded history of one window: cycles oldest first, samples
// oldest first within a cycle.
type ring struct {
	cycles []model.Cycle
}

// record appends one observed reading. A reading whose reset instant differs
// from the current cycle's by more than resetTolerance opens a new cycle; one
// whose utilization matches the last two samples extends the flat run by
// moving its end forward; one within historyFineStep of the last sample
// replaces it. Readings out of order, with no instant or with a non-finite
// utilization are dropped.
func (r *ring) record(at time.Time, w model.Window) {
	r.put(at, w, false)
}

// put records one reading, observed or estimated. An estimated reading opens
// or extends an estimated cycle. The first observed reading into an estimated
// cycle is appended whole, never merged into the estimate, and moves the cycle
// from Estimated to EstimatedUntil at the last estimated sample. An estimated
// reading behind an observed one in the same cycle is recorded as observed:
// the estimate is only ever a prefix.
func (r *ring) put(at time.Time, w model.Window, estimated bool) {
	if at.IsZero() || math.IsNaN(w.Utilization) || math.IsInf(w.Utilization, 0) {
		return
	}
	s := model.Sample{At: at.Truncate(time.Second), Utilization: math.Round(w.Utilization*utilizationScale) / utilizationScale}

	n := len(r.cycles)
	if n == 0 || rolled(r.cycles[n-1].ResetsAt, w.ResetsAt) {
		r.cycles = append(r.cycles, model.Cycle{ResetsAt: w.ResetsAt, Samples: []model.Sample{s}, Estimated: estimated})
		r.compact()
		return
	}
	c := &r.cycles[n-1]
	if c.ResetsAt.IsZero() {
		c.ResetsAt = w.ResetsAt
	}
	last := &c.Samples[len(c.Samples)-1]
	switch {
	case !s.At.After(last.At):
		return
	case c.Estimated && !estimated:
		until := last.At
		c.Estimated, c.EstimatedUntil = false, &until
		c.Samples = append(c.Samples, s)
	case s.At.Sub(last.At) < historyFineStep:
		*last = s
	case len(c.Samples) >= 2 && last.Utilization == s.Utilization && c.Samples[len(c.Samples)-2].Utilization == s.Utilization:
		last.At = s.At
	default:
		c.Samples = append(c.Samples, s)
	}
	r.compact()
}

// rolled reports whether a reading's reset instant belongs to a later (or
// otherwise different) window than the cycle's. A reading with no instant
// stays in the current cycle, as does a cycle that never learned one.
func rolled(cycle, reading time.Time) bool {
	if cycle.IsZero() || reading.IsZero() {
		return false
	}
	return reading.Sub(cycle).Abs() > resetTolerance
}

// compact applies the coarse tier and the cap. Every sample older than
// historyFineSpan before the newest keeps only the last reading in its
// historyCoarseStep bucket; then the oldest samples go until the ring is
// within historyMaxSamples, and a cycle emptied that way goes with them.
func (r *ring) compact() {
	if len(r.cycles) == 0 {
		return
	}
	newest := r.cycles[len(r.cycles)-1]
	edge := newest.Samples[len(newest.Samples)-1].At.Add(-historyFineSpan)
	total := 0
	for i := range r.cycles {
		c := &r.cycles[i]
		if len(c.Samples) > 0 && c.Samples[0].At.Before(edge) {
			c.Samples = coarsen(c.Samples, edge)
		}
		total += len(c.Samples)
	}
	for total > historyMaxSamples {
		c := &r.cycles[0]
		drop := min(total-historyMaxSamples, len(c.Samples))
		c.Samples = c.Samples[drop:]
		total -= drop
		if len(c.Samples) == 0 {
			r.cycles = r.cycles[1:]
		} else if c.EstimatedUntil != nil && c.Samples[0].At.After(*c.EstimatedUntil) {
			// Every estimated sample is gone; what remains is observed.
			c.EstimatedUntil = nil
		}
	}
}

// coarsen keeps one sample per historyCoarseStep bucket among those before
// edge — the last in each bucket — and every sample from edge on.
func coarsen(samples []model.Sample, edge time.Time) []model.Sample {
	out := samples[:0]
	for i, s := range samples {
		if !s.At.Before(edge) {
			out = append(out, s)
			continue
		}
		if i+1 < len(samples) && samples[i+1].At.Before(edge) &&
			samples[i+1].At.Truncate(historyCoarseStep).Equal(s.At.Truncate(historyCoarseStep)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// export copies the ring as a WindowHistory holding at most max samples. A
// ring over the bound keeps every cycle's first and last sample and every k-th
// in between, so the shape and the boundaries survive the thinning. max <= 0
// means the whole ring.
func (r *ring) export(kind model.WindowKind, scope string, max int) model.WindowHistory {
	out := model.WindowHistory{Kind: kind, Scope: scope, Cycles: make([]model.Cycle, 0, len(r.cycles))}
	total := 0
	for _, c := range r.cycles {
		total += len(c.Samples)
	}
	stride := 1
	if max > 0 && total > max {
		stride = (total + max - 1) / max
	}
	for _, c := range r.cycles {
		kept := make([]model.Sample, 0, len(c.Samples)/stride+2)
		for i, s := range c.Samples {
			if i%stride == 0 || i == len(c.Samples)-1 {
				kept = append(kept, s)
			}
		}
		out.Cycles = append(out.Cycles, model.Cycle{ResetsAt: c.ResetsAt, Samples: kept, Estimated: c.Estimated, EstimatedUntil: c.EstimatedUntil})
	}
	return out
}

// replay records every sample of a stored history in order, each as the
// estimate or observation its cycle marks it, so a ring loaded from disk obeys
// the same spacing, tiering and cap as one built live.
func (r *ring) replay(h model.WindowHistory) {
	for _, c := range h.Cycles {
		for _, s := range c.Samples {
			r.put(s.At, model.Window{Kind: h.Kind, Scope: h.Scope, Utilization: s.Utilization, ResetsAt: c.ResetsAt}, c.SampleEstimated(s))
		}
	}
}
