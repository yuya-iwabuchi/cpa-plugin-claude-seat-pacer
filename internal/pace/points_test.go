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
