package pace

import (
	"math"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func pointsCfg(t *testing.T, entries ...string) model.PaceConfig {
	t.Helper()
	cfg := model.Defaults()
	cfg.Pace.Shape, cfg.Pace.Points = model.ShapePoints, entries
	if w := cfg.Normalize(); len(w) != 0 {
		t.Fatalf("Normalize(%q) warned %q", entries, w)
	}
	return cfg.Pace
}

// Each segment runs as its kind says, every point is on the curve, and the
// landing and HoldAtFull do not reach a points curve.
func TestPointsTarget(t *testing.T) {
	cfg := pointsCfg(t, "0:0", "3.5:30", "5:100 smooth", "6:100 straight", "7:150 step")
	cfg.LandingTarget, cfg.HoldAtFull = 1.1, true
	for _, tc := range []struct{ e, want float64 }{
		{0, 0}, {0.5, 0.3}, {5.0 / 7, 1}, {5.5 / 7, 1}, {6.0 / 7, 1}, {6.5 / 7, 1}, {1, 1.5},
	} {
		if got := Target(cfg, tc.e); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Target at %.3f = %v, want %v", tc.e, got, tc.want)
		}
	}
	straight := pointsCfg(t, "7:140 straight")
	if got := Target(straight, 0.5); math.Abs(got-0.7) > 1e-9 {
		t.Errorf("straight midpoint = %v, want 0.7", got)
	}
}

// A smooth curve never falls and never leaves the band between the points
// either side of it, however uneven the points.
func TestSmoothPointsStayMonotone(t *testing.T) {
	cfg := pointsCfg(t, "0.5:40", "1:41", "4:42", "4.2:100", "7:101")
	prev := -1.0
	for i := 0; i <= 700; i++ {
		e := float64(i) / 700
		got := Target(cfg, e)
		if got < prev-1e-12 {
			t.Fatalf("curve falls at %.4f: %v after %v", e, got, prev)
		}
		if got < 0 || got > 1.01+1e-12 {
			t.Fatalf("curve at %.4f = %v, outside its points", e, got)
		}
		prev = got
	}
}

// A guided stretch passes through its two points, bends toward its guide the
// more it pulls, runs straight with its guide on the chord, and never falls.
func TestGuidedPointsTarget(t *testing.T) {
	at := func(cfg model.PaceConfig, e float64) float64 { return Target(cfg, e) }
	chord := pointsCfg(t, "7:100 via 3.5:50")
	for _, e := range []float64{0, 0.25, 0.5, 0.8, 1} {
		if got := at(chord, e); math.Abs(got-e) > 1e-9 {
			t.Errorf("guide on the chord: Target at %v = %v, want %v", e, got, e)
		}
	}
	late := pointsCfg(t, "7:100 via 7:0")
	if got, want := at(late, 0.5), 1.5-math.Sqrt2; math.Abs(got-want) > 1e-9 {
		t.Errorf("guide at the late corner: midpoint %v, want %v", got, want)
	}
	weak, strong := pointsCfg(t, "7:100 via 7:0 pull 0.5"), pointsCfg(t, "7:100 via 7:0 pull 4")
	if !(at(strong, 0.5) < at(late, 0.5) && at(late, 0.5) < at(weak, 0.5) && at(weak, 0.5) < 0.5) {
		t.Errorf("pull order: strong %v, plain %v, weak %v, want rising and under 0.5", at(strong, 0.5), at(late, 0.5), at(weak, 0.5))
	}
	mixed := pointsCfg(t, "2:10 via 2:0 pull 10", "4:90 via 2:90 pull 0.1", "5:95", "7:120 via 5:120 pull 3")
	prev := -1.0
	for i := 0; i <= 1400; i++ {
		e := float64(i) / 1400
		got := at(mixed, e)
		if got < prev-1e-12 {
			t.Fatalf("guided curve falls at %.4f: %v after %v", e, got, prev)
		}
		prev = got
	}
	for _, p := range []struct{ e, want float64 }{{2.0 / 7, 0.1}, {4.0 / 7, 0.9}, {5.0 / 7, 0.95}, {1, 1.2}} {
		if got := at(mixed, p.e); math.Abs(got-p.want) > 1e-9 {
			t.Errorf("guided curve misses its point at %v: %v, want %v", p.e, got, p.want)
		}
	}
}
