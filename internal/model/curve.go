package model

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// CurvePoint is one point of a points-shaped pace curve: X is the fraction of
// the window elapsed, Y the utilization the target reaches there, and Kind how
// the curve runs into the point from the one before it. The first point has
// no Kind. A guided stretch carries its Guide.
type CurvePoint struct {
	X     float64     `json:"x"`
	Y     float64     `json:"y"`
	Kind  string      `json:"kind,omitempty"`
	Guide *CurveGuide `json:"guide,omitempty"`
}

// CurveGuide is the point a guided stretch bends toward, in the curve's units,
// and Pull, the weight of its rational quadratic Bézier: 1 is the plain
// quadratic, more draws the stretch closer to the guide, less straightens it.
// The guide lies inside the box its stretch's two points span, so the stretch
// never falls and never runs back in time.
type CurveGuide struct {
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Pull float64 `json:"pull"`
}

// How a points curve runs from one point to the next.
const (
	// SegmentSmooth follows a monotone cubic through the points, which never
	// dips below or overshoots the points either side.
	SegmentSmooth = "smooth"
	// SegmentStraight is a straight line between the two points.
	SegmentStraight = "straight"
	// SegmentStep holds the earlier point's value up to the later point,
	// and steps up there.
	SegmentStep = "step"
	// SegmentGuided bends toward the stretch's CurveGuide.
	SegmentGuided = "guided"
)

const (
	// CurveDays is the span a pace.points day counts across: a window read as
	// a 7-day week. A 5-hour window follows the same curve, scaled.
	CurveDays = 7
	// MaxCurvePoints bounds a points curve, the points the plugin adds to its
	// ends included.
	MaxCurvePoints = 16
	// maxCurveY is a points curve's highest target, as LandingTarget's bound.
	maxCurveY = 4
	// MinGuidePull and MaxGuidePull bound a guide's pull.
	MinGuidePull = 0.1
	MaxGuidePull = 10
)

var pointEntry = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?|\.\d+)\s*:\s*(\d+(?:\.\d+)?|\.\d+)\s*%?` +
	`(?:\s+([A-Za-z]+))?` +
	`(?:\s+via\s+(\d+(?:\.\d+)?|\.\d+)\s*:\s*(\d+(?:\.\d+)?|\.\d+)\s*%?(?:\s+pull\s+(\d+(?:\.\d+)?|\.\d+))?)?$`)

// ParsePoints reads pace.points: entries "day:percent", each optionally
// followed by how the curve runs into it, "smooth" (the default), "straight"
// or "step", or by "via day:percent", a guide the stretch into the point
// bends toward, itself optionally followed by "pull" and a weight from 0.1 to
// 10 (1 when left out). Days run from 0 to CurveDays and percents from 0 to
// 400; days rise strictly and percents never fall, and a guide lies between
// its stretch's two points in both. A curve that does not start on day 0
// starts there at 0%, and one that stops short of the last day holds its last
// value to it.
func ParsePoints(entries []string) ([]CurvePoint, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("names no points")
	}
	out := make([]CurvePoint, 0, len(entries)+2)
	for _, raw := range entries {
		m := pointEntry.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			return nil, fmt.Errorf("%q is not day:percent with an optional smooth, straight, step or via day:percent", raw)
		}
		day, _ := strconv.ParseFloat(m[1], 64)
		percent, _ := strconv.ParseFloat(m[2], 64)
		kind := strings.ToLower(m[3])
		var guide *CurveGuide
		if m[4] != "" {
			if kind != "" {
				return nil, fmt.Errorf("%q names both %q and a guide", raw, m[3])
			}
			gd, _ := strconv.ParseFloat(m[4], 64)
			gp, _ := strconv.ParseFloat(m[5], 64)
			pull := 1.0
			if m[6] != "" {
				pull, _ = strconv.ParseFloat(m[6], 64)
				if pull < MinGuidePull || pull > MaxGuidePull {
					return nil, fmt.Errorf("%q pulls %v, outside %v to %v", raw, pull, MinGuidePull, MaxGuidePull)
				}
			}
			guide = &CurveGuide{X: gd / CurveDays, Y: gp / 100, Pull: pull}
			kind = SegmentGuided
		}
		switch kind {
		case "", SegmentGuided:
			if kind == "" {
				kind = SegmentSmooth
			}
		case "linear":
			kind = SegmentStraight
		case SegmentSmooth, SegmentStraight, SegmentStep:
		default:
			return nil, fmt.Errorf("%q names %q, not smooth, straight or step", raw, m[3])
		}
		if day > CurveDays {
			return nil, fmt.Errorf("%q is past day %d", raw, CurveDays)
		}
		if percent > maxCurveY*100 {
			return nil, fmt.Errorf("%q is above %d%%", raw, maxCurveY*100)
		}
		p := CurvePoint{X: day / CurveDays, Y: percent / 100, Kind: kind, Guide: guide}
		if n := len(out); n > 0 {
			if p.X <= out[n-1].X {
				return nil, fmt.Errorf("%q does not come after the point before it", raw)
			}
			if p.Y < out[n-1].Y {
				return nil, fmt.Errorf("%q falls below the point before it", raw)
			}
		}
		out = append(out, p)
	}
	switch {
	case out[0].X > 0:
		out = append([]CurvePoint{{}}, out...)
	case out[0].Y > 0:
		return nil, fmt.Errorf("starts above 0%% on day 0")
	case out[0].Guide != nil:
		return nil, fmt.Errorf("guides the point on day 0, which no stretch runs into")
	}
	out[0].Kind = ""
	for k := 1; k < len(out); k++ {
		a, b, g := out[k-1], out[k], out[k].Guide
		if g != nil && (g.X < a.X || g.X > b.X || g.Y < a.Y || g.Y > b.Y) {
			return nil, fmt.Errorf("guide %v:%v%% is outside the stretch from day %v to day %v", fmtDay(g.X), fmtPct(g.Y), fmtDay(a.X), fmtDay(b.X))
		}
	}
	if last := out[len(out)-1]; last.X < 1 {
		out = append(out, CurvePoint{X: 1, Y: last.Y, Kind: SegmentStraight})
	}
	if len(out) > MaxCurvePoints {
		return nil, fmt.Errorf("has %d points, more than %d", len(out), MaxCurvePoints)
	}
	return out, nil
}

func fmtDay(x float64) string {
	return strconv.FormatFloat(math.Round(x*CurveDays*100)/100, 'f', -1, 64)
}
func fmtPct(y float64) string { return strconv.FormatFloat(math.Round(y*1e4)/100, 'f', -1, 64) }
