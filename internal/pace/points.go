package pace

import (
	"math"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// pointsAt is the points curve's target at elapsed, which lies in [0, 1].
// Each segment runs as its later point's Kind says; a smooth one is the cubic
// Hermite through its two points with the tangents curveTangents gives, and a
// guided one the rational quadratic Bézier guidedAt gives.
func pointsAt(pts []model.CurvePoint, elapsed float64) float64 {
	n := len(pts)
	if elapsed <= pts[0].X {
		return pts[0].Y
	}
	if elapsed >= pts[n-1].X {
		return pts[n-1].Y
	}
	k := 0
	for k < n-2 && elapsed >= pts[k+1].X {
		k++
	}
	a, b := pts[k], pts[k+1]
	h := b.X - a.X
	t := (elapsed - a.X) / h
	switch b.Kind {
	case model.SegmentStep:
		return a.Y
	case model.SegmentStraight:
		return a.Y + (b.Y-a.Y)*t
	case model.SegmentGuided:
		if b.Guide != nil {
			return guidedAt(a, b, *b.Guide, elapsed)
		}
	}
	m := curveTangents(pts)
	t2, t3 := t*t, t*t*t
	return (2*t3-3*t2+1)*a.Y + (t3-2*t2+t)*h*m[k] + (-2*t3+3*t2)*b.Y + (t3-t2)*h*m[k+1]
}

// curveTangents are the Fritsch–Carlson tangents at each point: the mean of
// the secants either side, zero where the curve is flat on either side, and
// scaled down where they would overshoot, so every smooth segment rises
// monotonically from one point to the next.
func curveTangents(pts []model.CurvePoint) []float64 {
	n := len(pts)
	d := make([]float64, n-1)
	for k := 0; k < n-1; k++ {
		d[k] = (pts[k+1].Y - pts[k].Y) / (pts[k+1].X - pts[k].X)
	}
	m := make([]float64, n)
	m[0], m[n-1] = d[0], d[n-2]
	for k := 1; k < n-1; k++ {
		if d[k-1] > 0 && d[k] > 0 {
			m[k] = (d[k-1] + d[k]) / 2
		}
	}
	for k := 0; k < n-1; k++ {
		if d[k] == 0 {
			m[k], m[k+1] = 0, 0
			continue
		}
		a, b := m[k]/d[k], m[k+1]/d[k]
		if s := a*a + b*b; s > 9 {
			tau := 3 / math.Sqrt(s)
			m[k], m[k+1] = tau*a*d[k], tau*b*d[k]
		}
	}
	return m
}

// guidedAt is the height at x of the rational quadratic Bézier from a to b
// with control point g weighted g.Pull. Its guide lies inside the box a and b
// span, so x rises with the parameter, which bisection finds; the page runs
// the same steps, so both read the same value.
func guidedAt(a, b model.CurvePoint, g model.CurveGuide, x float64) float64 {
	at := func(t float64, p0, p1, p2 float64) float64 {
		u := 1 - t
		return (u*u*p0 + 2*g.Pull*u*t*p1 + t*t*p2) / (u*u + 2*g.Pull*u*t + t*t)
	}
	if x <= a.X {
		return a.Y
	}
	if x >= b.X {
		return b.Y
	}
	lo, hi := 0.0, 1.0
	for i := 0; i < 52; i++ {
		mid := (lo + hi) / 2
		if at(mid, a.X, g.X, b.X) < x {
			lo = mid
		} else {
			hi = mid
		}
	}
	return at((lo+hi)/2, a.Y, g.Y, b.Y)
}
