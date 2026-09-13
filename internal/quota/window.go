package quota

import (
	"strconv"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// resetTolerance absorbs clock skew between this host and Anthropic when
// deciding whether a reset instant belongs to the window carrying it.
const resetTolerance = 2 * time.Minute

// windowKey identifies one reading within one credential.
type windowKey struct {
	kind  model.WindowKind
	scope string
}

func keyOf(w model.Window) windowKey {
	return windowKey{kind: w.Kind, scope: w.Scope}
}

// windowSet accumulates readings in insertion order, so parsing is
// deterministic and a later observation can enrich an earlier one.
type windowSet struct {
	order []windowKey
	byKey map[windowKey]*model.Window
}

func newWindowSet() *windowSet {
	return &windowSet{byKey: make(map[windowKey]*model.Window)}
}

// at returns the reading for an identity, creating an empty one when absent.
func (s *windowSet) at(kind model.WindowKind, scope string) *model.Window {
	key := windowKey{kind: kind, scope: scope}
	if w, ok := s.byKey[key]; ok {
		return w
	}
	w := &model.Window{Kind: kind, Scope: scope}
	s.byKey[key] = w
	s.order = append(s.order, key)
	return w
}

func (s *windowSet) lookup(kind model.WindowKind, scope string) (*model.Window, bool) {
	w, ok := s.byKey[windowKey{kind: kind, scope: scope}]
	return w, ok
}

func (s *windowSet) slice() []model.Window {
	if len(s.order) == 0 {
		return nil
	}
	out := make([]model.Window, 0, len(s.order))
	for _, key := range s.order {
		out = append(out, *s.byKey[key])
	}
	return out
}

// hasKey reports whether a window carries an identity, for slices.IndexFunc.
func hasKey(key windowKey) func(model.Window) bool {
	return func(w model.Window) bool { return keyOf(w) == key }
}

// instantLayouts covers the textual reset formats Anthropic and intermediaries
// emit. RFC3339 also accepts the fractional seconds the usage endpoint writes;
// RFC1123 covers the HTTP-date a proxy puts on a response.
var instantLayouts = []string{
	time.RFC3339,
	time.RFC1123,
}

// epochMillisCutoff tells a millisecond epoch from a second one. A
// second-valued epoch stays below it until the year 5138 and a
// millisecond-valued one passes it from 1973 on, so no reset instant a live
// provider emits is ambiguous.
const epochMillisCutoff = 1e11

// parseInstant reads a reset instant in any form the provider emits: a unix
// epoch on the response headers, and a timestamp string on the usage endpoint.
func parseInstant(raw string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n >= epochMillisCutoff {
			return time.UnixMilli(n).UTC(), true
		}
		return time.Unix(n, 0).UTC(), true
	}
	for _, layout := range instantLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// windowReset reports raw as the reset of a window of duration d, and reports
// the zero time for an instant outside the window it claims to describe. Such
// a value cannot belong to this window, so the reading keeps its utilization
// and loses only its timeline, which Window.Elapsed already reports as 0.
func windowReset(raw string, now time.Time, d time.Duration) time.Time {
	t, ok := parseInstant(raw)
	if !ok {
		return time.Time{}
	}
	if t.Before(now.Add(-resetTolerance)) || t.After(now.Add(d+resetTolerance)) {
		return time.Time{}
	}
	return t
}
