package runtime

import (
	"sync"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// decisionLog is a bounded ring of routing decisions. The pick path appends
// under a mutex held only for the slice write; readers get a copy, newest
// first, so the status view never observes a partially written entry.
type decisionLog struct {
	mu      sync.Mutex
	entries []model.Decision
	next    int
	full    bool
}

func newDecisionLog(limit int) *decisionLog {
	if limit <= 0 {
		limit = 1
	}
	return &decisionLog{entries: make([]model.Decision, limit)}
}

// add appends one decision, overwriting the oldest once the ring is full.
func (l *decisionLog) add(d model.Decision) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = d
	l.next = (l.next + 1) % len(l.entries)
	if l.next == 0 {
		l.full = true
	}
}

// newestFirst returns every retained decision, most recent first.
func (l *decisionLog) newestFirst() []model.Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := l.next
	if l.full {
		count = len(l.entries)
	}
	out := make([]model.Decision, 0, count)
	for i := 1; i <= count; i++ {
		idx := (l.next - i + len(l.entries)) % len(l.entries)
		out = append(out, l.entries[idx])
	}
	return out
}

// resize changes the capacity, keeping the newest entries that fit. A resize
// to the current capacity is a no-op, so a reconfigure that leaves the limit
// alone costs nothing.
func (l *decisionLog) resize(limit int) {
	if limit <= 0 {
		limit = 1
	}
	l.mu.Lock()
	if limit == len(l.entries) {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	kept := l.newestFirst()
	if len(kept) > limit {
		kept = kept[:limit]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make([]model.Decision, limit)
	l.next = 0
	l.full = false
	for i := len(kept) - 1; i >= 0; i-- {
		l.entries[l.next] = kept[i]
		l.next = (l.next + 1) % limit
		if l.next == 0 {
			l.full = true
		}
	}
}
