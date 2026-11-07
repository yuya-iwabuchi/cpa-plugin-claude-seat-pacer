package model

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CurvePoint is one point of a points-shaped pace curve: X is the fraction of
// the window elapsed, Y the utilization the target reaches there, and Kind how
// the curve runs into the point from the one before it. The first point has
// no Kind.
type CurvePoint struct {
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Kind string  `json:"kind,omitempty"`
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
)

var pointEntry = regexp.MustCompile(`^(\d+(?:\.\d+)?|\.\d+)\s*:\s*(\d+(?:\.\d+)?|\.\d+)\s*%?(?:\s+([A-Za-z]+))?$`)

// ParsePoints reads pace.points: entries "day:percent", each optionally
// followed by how the curve runs into it, "smooth" (the default), "straight"
// or "step". Days run from 0 to CurveDays and percents from 0 to 400; days
// rise strictly and percents never fall. A curve that does not start on day
// 0 starts there at 0%, and one that stops short of the last day holds its
// last value to it.
func ParsePoints(entries []string) ([]CurvePoint, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("names no points")
	}
	out := make([]CurvePoint, 0, len(entries)+2)
	for _, raw := range entries {
		m := pointEntry.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			return nil, fmt.Errorf("%q is not day:percent with an optional smooth, straight or step", raw)
		}
		day, _ := strconv.ParseFloat(m[1], 64)
		percent, _ := strconv.ParseFloat(m[2], 64)
		kind := strings.ToLower(m[3])
		switch kind {
		case "":
			kind = SegmentSmooth
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
		p := CurvePoint{X: day / CurveDays, Y: percent / 100, Kind: kind}
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
	}
	out[0].Kind = ""
	if last := out[len(out)-1]; last.X < 1 {
		out = append(out, CurvePoint{X: 1, Y: last.Y, Kind: SegmentStraight})
	}
	if len(out) > MaxCurvePoints {
		return nil, fmt.Errorf("has %d points, more than %d", len(out), MaxCurvePoints)
	}
	return out, nil
}
