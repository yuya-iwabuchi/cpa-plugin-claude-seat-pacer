package quota

import (
	"math"
	"slices"
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
	// historyMaxLocks bounds the refusal spans a window keeps; past it the
	// oldest go.
	historyMaxLocks = 64
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

// carryOverSpan is how long past the previous window's reset a reading at
// that window's level is taken for the provider still reporting it under the
// new reset, rather than for use in the fresh one; carryDrift is how far
// under that level the provider's late answers for the old window have been
// seen to sit.
const (
	carryOverSpan = 10 * time.Minute
	carryDrift    = 0.02
)

// ring is the recorded history of one window: cycles oldest first, samples
// oldest first within a cycle.
type ring struct {
	cycles []model.Cycle
	// replaying is set while addCycles reads recorded cycles, whose samples
	// inside a cycle were already accepted live.
	replaying bool
	// fall holds a reading that fell clearDrop under the cycle's level, until
	// the next reading confirms the clearing or shows it stale, or the cycle
	// rolls or closes. It is never exported, so a restart forgets an
	// unconfirmed fall.
	fall *pendingFall
	// locks are the spans in which the provider refused the window, oldest
	// first and disjoint. A span ends where a request the window caps was
	// served, where the provider cleared the window early, or at a reset:
	// when a cycle rolls or closes, or when its expected reset passes. Until
	// then it is ongoing and runs to the reset its latest refusal expected.
	// Only the newest span can be ongoing.
	locks []lock
	// refused is when the newest span's latest refusal was admitted, at full
	// precision. It is never exported; a ring loaded from disk holds the load
	// instant instead, which every request admitted since is after, or the
	// live ring's own where an import merged live spans into it.
	refused time.Time
	// served is the newest admission of a request the window caps that was
	// served, at full precision. It is never exported.
	served time.Time
}

// lock is one refusal span, at second precision. end is empty while it is
// ongoing.
type lock struct {
	from, to time.Time
	end      model.LockEnd
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
// Around a reset the provider can answer from both windows for a few
// minutes. A reading stamped with a reset earlier than the current cycle's,
// and already past, describes the window that ended and is dropped; so is
// one within carryOverSpan of the previous window's reset that jumps back to
// that window's level under the new reset. A replay applies that second test
// only to the reading of a one-reading cycle addCycles folds in, since every
// other stored sample was already accepted when it was recorded.
//
// Readings out of order, with no instant or with a non-finite utilization
// are dropped. Utilization only rises until the window resets or the
// provider clears it, and the two sources that feed a ring round
// differently, so an observed reading clearDrop or less under an observed
// last sample is the coarser source lagging the finer one: it is recorded at
// the last sample's level, so the flat run still ends at the newest reading.
// One lower than an estimated sample is dropped, since an estimate's level
// can run high.
//
// A cycle that opens after another ends an ongoing refusal span: as reset at
// the reading that rolled or closed it, or as cleared at a clearing's first
// fallen reading.
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
	if n > 0 && endedBefore(r.cycles[n-1].ResetsAt, w.ResetsAt, s.At) {
		return false
	}
	if n == 0 || rolled(r.cycles[n-1].ResetsAt, w.ResetsAt) || closed(r.cycles[n-1], s.At) {
		// A cycle opened by a reading the provider is still stamping with the
		// expired reset inherits that stale instant; the first reading to
		// carry the real one rolls the cycle onto it.
		if n > 0 {
			r.end(s.At, model.LockEndReset)
		}
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
		r.end(r.fall.sample.At, model.LockEndCleared)
		r.fall = nil
		r.cycles = append(r.cycles, model.Cycle{ResetsAt: w.ResetsAt, Samples: []model.Sample{s}})
		return true
	default:
		r.end(r.fall.sample.At, model.LockEndCleared)
		r.cycles = append(r.cycles, model.Cycle{ResetsAt: r.fall.resetsAt, Samples: []model.Sample{r.fall.sample}})
		r.fall = nil
	}
	c := &r.cycles[len(r.cycles)-1]
	if c.ResetsAt.IsZero() {
		c.ResetsAt = w.ResetsAt
	}
	if !estimated && !r.replaying && r.carriedOver(s) {
		return false
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

// endedBefore reports whether a reading taken at `at` describes a window that
// ended before the cycle's: its reset is more than resetTolerance earlier
// than the cycle's, and already past.
func endedBefore(cycle, reading, at time.Time) bool {
	if cycle.IsZero() || reading.IsZero() {
		return false
	}
	return cycle.Sub(reading) > resetTolerance && !reading.After(at)
}

// carriedOver reports whether an observed reading in the newest cycle is the
// previous window's level reported under the new reset: the reading is
// within carryOverSpan of the previous cycle's reset, the newest cycle opened
// at that reset, and the reading climbs more than clearDrop over the cycle's
// last sample to the level the previous cycle ended at, or up to carryDrift
// under it.
func (r *ring) carriedOver(s model.Sample) bool {
	if !r.nearRoll(s.At) {
		return false
	}
	prev, c := r.cycles[len(r.cycles)-2], r.cycles[len(r.cycles)-1]
	level := prev.Samples[len(prev.Samples)-1].Utilization
	last := c.Samples[len(c.Samples)-1].Utilization
	under := math.Round((level - s.Utilization) * utilizationScale)
	return s.Utilization-last > clearDrop && under >= 0 && under <= carryDrift*utilizationScale
}

// nearRoll reports whether the newest cycle opened at the previous cycle's
// reset and `at` is within carryOverSpan of that reset. `at` is measured from
// the reset instant, which compaction never moves.
func (r *ring) nearRoll(at time.Time) bool {
	n := len(r.cycles)
	if n < 2 {
		return false
	}
	prev, c := r.cycles[n-2], r.cycles[n-1]
	if prev.ResetsAt.IsZero() || c.Estimated {
		return false
	}
	opened := c.Samples[0].At.Sub(prev.ResetsAt)
	return opened >= -resetTolerance && opened <= carryOverSpan && at.Sub(prev.ResetsAt) <= carryOverSpan
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
// within historyMaxSamples, and a cycle emptied that way goes with them. The
// refusal spans are trimmed to what is left.
func (r *ring) compact() {
	defer r.trimLocks()
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
// means the whole ring. Every refusal span is copied whatever max is; an
// ongoing one whose expected reset is not after now is copied as ended there
// by the reset.
func (r *ring) export(kind model.WindowKind, scope string, max int, now time.Time) model.WindowHistory {
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
	if len(r.locks) > 0 {
		out.Locks = make([]model.Lock, len(r.locks))
		for i, l := range r.locks {
			if l.end == "" && !l.to.After(now) {
				l.end = model.LockEndReset
			}
			out.Locks[i] = model.Lock{From: l.from.Unix(), To: l.to.Unix(), End: l.end}
		}
	}
	return out
}

// replay records every sample of a stored history in order, each as the
// estimate or observation its cycle marks it, so a ring loaded from disk obeys
// the same spacing, tiering and cap as one built live. The tiering and the cap
// run once, after the last sample: compaction never drops the newest cycle's
// last two samples, the only ones a following reading is compared against.
//
// The stored refusal spans are restored as they were written, and loaded
// stands in for the admission of the newest span's latest refusal: every
// stored refusal came before the load.
func (r *ring) replay(h model.WindowHistory, loaded time.Time) {
	r.addCycles(h.Kind, h.Scope, h.Cycles)
	for _, l := range h.Locks {
		r.mergeLock(lock{from: time.Unix(l.From, 0).UTC(), to: time.Unix(l.To, 0).UTC(), end: l.End})
	}
	if len(r.locks) > 0 {
		r.refused = loaded
	}
	r.compact()
}

// addCycles records recorded cycles in order without compacting. The first
// cycle's samples pass through add like live readings, so they can continue
// the ring's newest cycle. A later cycle opens a cycle of its own, since its
// boundary was already decided when it was recorded, except within
// carryOverSpan of a reset: there an observed one-reading cycle under the
// ring's newest cycle's reset that neither closes nor clears it passes through
// add too, so a reset the provider answered from both windows reads as the one
// roll it was. A cycle of several readings, or an estimated one, keeps its
// boundary, since the reading that opened it may be one a later reading
// replaced. A later cycle for a window that ended before the ring's newest, or
// whose first sample is not after the ring's newest, is dropped whole, like
// any reading out of order.
func (r *ring) addCycles(kind model.WindowKind, scope string, cycles []model.Cycle) {
	r.replaying = true
	defer func() { r.replaying = false }()
	for i, c := range cycles {
		for j, s := range c.Samples {
			w := model.Window{Kind: kind, Scope: scope, Utilization: s.Utilization, ResetsAt: c.ResetsAt}
			estimated := c.SampleEstimated(s)
			if i == 0 || j > 0 {
				r.add(s.At, w, estimated)
				continue
			}
			if n := len(r.cycles); n > 0 {
				cur := r.cycles[n-1]
				newest := cur.Samples
				if !s.At.After(newest[len(newest)-1].At) || endedBefore(cur.ResetsAt, c.ResetsAt, s.At) {
					break
				}
				if !estimated && len(c.Samples) == 1 && r.nearRoll(s.At) && !rolled(cur.ResetsAt, c.ResetsAt) && !closed(cur, s.At) && !cleared(cur, s, false) {
					if !r.carriedOver(model.Sample{At: s.At.Truncate(time.Second), Utilization: s.Utilization}) {
						r.add(s.At, w, estimated)
					}
					continue
				}
			}
			r.fall = nil
			r.cycles = append(r.cycles, model.Cycle{ResetsAt: c.ResetsAt, Samples: []model.Sample{s}, Estimated: estimated})
		}
	}
}

// refuse records a refusal at `at` of a request admitted at `admitted`, which
// the provider expects to last until `until`, and reports whether the spans
// changed. A refusal admitted at or before the newest served admission is
// stale, since the window admitted a later request, and changes nothing. A
// refusal inside the newest span runs it to `until`, reopening it where a
// served request had ended it; one inside a span a reset or a clearing ended
// leaves it ended; one before the span's start moves the start back to it;
// one at or after the span's end opens a span of its own. One with no end
// after `at`, or reaching back into an older span, is dropped.
func (r *ring) refuse(admitted, at, until time.Time) bool {
	at, until = at.Truncate(time.Second), until.Truncate(time.Second)
	if !until.After(at) || !admitted.After(r.served) {
		return false
	}
	n := len(r.locks)
	if n == 0 || !at.Before(r.locks[n-1].to) {
		r.push(lock{from: at, to: until})
		r.refused = admitted
		r.trimLocks()
		return true
	}
	last := &r.locks[n-1]
	changed := false
	switch {
	case at.Before(last.from):
		if n > 1 && at.Before(r.locks[n-2].to) {
			return false
		}
		last.from, changed = at, true
	case last.end == "" || last.end == model.LockEndServed:
		changed = last.to != until || last.end != ""
		last.to, last.end = until, ""
	}
	if admitted.After(r.refused) {
		r.refused = admitted
	}
	return changed
}

// serve records a request admitted at `admitted` whose response began at
// `at`, and reports whether the spans changed. It keeps the newest served
// admission. A request admitted after the newest span's latest refusal ends
// that span at `at` as served while it is ongoing, and drops it when `at` is
// at or before the second the span opened; one admitted at or before that
// refusal was in flight while the window refused, so it leaves the span
// running.
func (r *ring) serve(admitted, at time.Time) bool {
	if admitted.After(r.served) {
		r.served = admitted
	}
	n := len(r.locks)
	if n == 0 || r.locks[n-1].end != "" || !admitted.After(r.refused) {
		return false
	}
	if !at.Truncate(time.Second).After(r.locks[n-1].from) {
		r.locks = r.locks[:n-1]
		return true
	}
	return r.end(at, model.LockEndServed)
}

// end ends the newest span at `at` for cause, when it is ongoing and began
// before `at`. A span whose expected reset `at` has reached keeps that end
// and ends as reset. It reports whether the spans changed.
func (r *ring) end(at time.Time, cause model.LockEnd) bool {
	at = at.Truncate(time.Second)
	n := len(r.locks)
	if n == 0 {
		return false
	}
	last := &r.locks[n-1]
	if last.end != "" || !at.After(last.from) {
		return false
	}
	if at.Before(last.to) {
		last.to, last.end = at, cause
	} else {
		last.end = model.LockEndReset
	}
	return true
}

// push appends a span that begins at or after the newest one's end. A newest
// span still ongoing then ends as reset, since the new span begins at or
// after the reset it expected.
func (r *ring) push(l lock) {
	if n := len(r.locks); n > 0 && r.locks[n-1].end == "" {
		r.locks[n-1].end = model.LockEndReset
	}
	r.locks = append(r.locks, l)
}

// mergeLock appends a recorded span behind the newest one. A span with no
// length is dropped; one overlapping the newest takes it over from the earlier
// start to its own end and cause, as the newer evidence of when and how the
// refusal ended; one that ends at or before the newest starts is dropped.
func (r *ring) mergeLock(l lock) {
	if !l.to.After(l.from) {
		return
	}
	n := len(r.locks)
	switch {
	case n == 0 || !l.from.Before(r.locks[n-1].to):
		r.push(l)
	case l.to.After(r.locks[n-1].from):
		last := &r.locks[n-1]
		if l.from.Before(last.from) {
			last.from = l.from
		}
		last.to, last.end = l.to, l.end
	}
}

// trimLocks drops every span that ended before the oldest sample the ring
// holds, then the oldest spans past historyMaxLocks.
func (r *ring) trimLocks() {
	drop := 0
	if len(r.cycles) > 0 && len(r.cycles[0].Samples) > 0 {
		oldest := r.cycles[0].Samples[0].At
		for drop < len(r.locks) && r.locks[drop].to.Before(oldest) {
			drop++
		}
	}
	drop = max(drop, len(r.locks)-historyMaxLocks)
	if drop > 0 {
		r.locks = slices.Delete(r.locks, 0, drop)
	}
}
