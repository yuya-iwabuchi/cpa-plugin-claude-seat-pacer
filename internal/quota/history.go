package quota

import (
	"math"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
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

// clearDrop is how far an observed reading has to fall under the observed
// sample before it, inside a cycle, to be a candidate for the provider
// clearing the window rather than one source lagging the other. The two
// sources differ by under a point of rounding; a clearing from below this
// level goes unseen, and the ring holds the old level until the window
// climbs back past it.
const clearDrop = 0.05

// ring is the recorded history of one window: cycles oldest first, samples
// oldest first within a cycle.
type ring struct {
	cycles []model.Cycle
	// fall holds a reading that fell clearDrop under the cycle's level, until
	// the next reading confirms the clearing or shows it stale, or the cycle
	// rolls or closes. It is never exported, so a restart forgets an
	// unconfirmed fall.
	fall *pendingFall
}

// pendingFall is a candidate clearing's first reading.
type pendingFall struct {
	sample   model.Sample
	resetsAt time.Time
}

// after reports the fall when it is newer than every sample the ring holds,
// and nil otherwise: a fall behind the ring's newest sample describes a level
// the ring has already moved past.
func (f *pendingFall) after(r *ring) *pendingFall {
	if f == nil || len(r.cycles) == 0 {
		return f
	}
	newest := r.cycles[len(r.cycles)-1].Samples
	if !f.sample.At.After(newest[len(newest)-1].At) {
		return nil
	}
	return f
}

// record appends one observed reading. A reading whose reset instant differs
// from the current cycle's by more than resetTolerance opens a new cycle, as
// does one taken more than resetTolerance past the current cycle's reset and
// one taken after the provider cleared the window; one whose utilization
// matches the last two samples extends the flat run by moving its end
// forward; one within historyFineStep of the last sample replaces it.
//
// A clearing takes two readings: the first to fall clearDrop under the
// cycle's level is held back, and it opens a cycle only when the next reading
// falls clearDrop under that level too, at the lower of the two when they
// differ by more than clearDrop. A single low reading followed by one at the
// level is stale — a response header describing the window as a long
// request found it, or an endpoint read behind the traffic — and is dropped.
//
// Readings out of order, with no instant or with a non-finite utilization
// are dropped. Utilization only rises until the window resets or the
// provider clears it, and the two sources that feed a ring round
// differently, so an observed reading clearDrop or less under an observed
// last sample is the coarser source lagging the finer one: it is recorded at
// the last sample's level, so the flat run still ends at the newest reading.
// One lower than an estimated sample is dropped, since an estimate's level
// can run high.
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
	if r.add(at, w, estimated) {
		r.compact()
	}
}

// add is put without the compaction, for a caller recording a run of readings
// that compacts once at the end. It reports whether the ring changed.
func (r *ring) add(at time.Time, w model.Window, estimated bool) bool {
	if at.IsZero() || math.IsNaN(w.Utilization) || math.IsInf(w.Utilization, 0) {
		return false
	}
	s := model.Sample{At: at.Truncate(time.Second), Utilization: math.Round(w.Utilization*utilizationScale) / utilizationScale}

	n := len(r.cycles)
	if n == 0 || rolled(r.cycles[n-1].ResetsAt, w.ResetsAt) || closed(r.cycles[n-1], s.At) {
		// A cycle opened by a reading the provider is still stamping with the
		// expired reset inherits that stale instant; the first reading to
		// carry the real one rolls the cycle onto it.
		r.fall = nil
		r.cycles = append(r.cycles, model.Cycle{ResetsAt: w.ResetsAt, Samples: []model.Sample{s}, Estimated: estimated})
		return true
	}
	switch {
	case !cleared(r.cycles[n-1], s, estimated):
		r.fall = nil
	case r.fall == nil:
		r.fall = &pendingFall{sample: s, resetsAt: w.ResetsAt}
		return false
	case !s.At.After(r.fall.sample.At):
		return false
	case r.fall.sample.Utilization-s.Utilization > clearDrop:
		r.fall = nil
		r.cycles = append(r.cycles, model.Cycle{ResetsAt: w.ResetsAt, Samples: []model.Sample{s}})
		return true
	default:
		r.cycles = append(r.cycles, model.Cycle{ResetsAt: r.fall.resetsAt, Samples: []model.Sample{r.fall.sample}})
		r.fall = nil
	}
	c := &r.cycles[len(r.cycles)-1]
	if c.ResetsAt.IsZero() {
		c.ResetsAt = w.ResetsAt
	}
	last := &c.Samples[len(c.Samples)-1]
	if s.Utilization < last.Utilization && !estimated && !c.SampleEstimated(*last) {
		s.Utilization = last.Utilization
	}
	switch {
	case !s.At.After(last.At):
		return false
	case s.Utilization < last.Utilization:
		return false
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
	return true
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

// closed reports whether a reading taken at `at` falls past the end of a
// cycle. A provider that keeps stamping the expired reset, or one whose reset
// windowReset drops for being too far in the past, gives a post-reset reading
// nothing for rolled to see, so the near-zero utilization of the fresh window
// would land in the cycle that just ended and draw a plunge to zero where the
// chart should break the line. The tolerance is the same one rolled uses, so
// clock skew alone never splits a cycle, and a cycle whose newest sample is
// already past the edge is the fresh one and splits no further.
func closed(c model.Cycle, at time.Time) bool {
	if c.ResetsAt.IsZero() || len(c.Samples) == 0 {
		return false
	}
	edge := c.ResetsAt.Add(resetTolerance)
	return at.After(edge) && !c.Samples[len(c.Samples)-1].At.After(edge)
}

// cleared reports whether an observed reading falls more than clearDrop under
// the cycle's last sample, itself observed. The provider can clear a window's
// usage early and keep its reset instant, which gives rolled and closed
// nothing to see; a confirmed fall opens a cycle of its own under the same
// reset, so every cycle only rises and the chart draws the fall between two.
func cleared(c model.Cycle, s model.Sample, estimated bool) bool {
	if estimated || len(c.Samples) == 0 {
		return false
	}
	last := c.Samples[len(c.Samples)-1]
	return !c.SampleEstimated(last) && s.At.After(last.At) && last.Utilization-s.Utilization > clearDrop
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
// the same spacing, tiering and cap as one built live. The tiering and the cap
// run once, after the last sample: compaction never drops the newest cycle's
// last two samples, the only ones a following reading is compared against.
func (r *ring) replay(h model.WindowHistory) {
	r.addCycles(h.Kind, h.Scope, h.Cycles)
	r.compact()
}

// addCycles records recorded cycles in order without compacting. The first
// cycle's samples pass through add like live readings, so they can continue
// the ring's newest cycle; every later cycle opens a cycle of its own, since
// its boundary was already decided when it was recorded. A later cycle whose
// first sample is not after the ring's newest is dropped whole, like any
// reading out of order.
func (r *ring) addCycles(kind model.WindowKind, scope string, cycles []model.Cycle) {
	for i, c := range cycles {
		for j, s := range c.Samples {
			w := model.Window{Kind: kind, Scope: scope, Utilization: s.Utilization, ResetsAt: c.ResetsAt}
			estimated := c.SampleEstimated(s)
			if i == 0 || j > 0 {
				r.add(s.At, w, estimated)
				continue
			}
			if n := len(r.cycles); n > 0 {
				newest := r.cycles[n-1].Samples
				if !s.At.After(newest[len(newest)-1].At) {
					break
				}
			}
			r.fall = nil
			r.cycles = append(r.cycles, model.Cycle{ResetsAt: c.ResetsAt, Samples: []model.Sample{s}, Estimated: estimated})
		}
	}
}
