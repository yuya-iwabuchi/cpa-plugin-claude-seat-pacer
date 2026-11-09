package model

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePoints(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      []string
		want    []CurvePoint
		wantErr string
	}{
		{name: "whole curve", in: []string{"0:0", "3.5:40", "7:110 straight"},
			want: []CurvePoint{{0, 0, "", nil}, {0.5, 0.4, SegmentSmooth, nil}, {1, 1.1, SegmentStraight, nil}}},
		{name: "ends added", in: []string{"5:100% Step"},
			want: []CurvePoint{{0, 0, "", nil}, {5.0 / 7, 1, SegmentStep, nil}, {1, 1, SegmentStraight, nil}}},
		{name: "linear is straight", in: []string{"7:100 linear"},
			want: []CurvePoint{{0, 0, "", nil}, {1, 1, SegmentStraight, nil}}},
		{name: "guided", in: []string{"3.5:20 straight", "7:100 via 7:20 pull 2"},
			want: []CurvePoint{{0, 0, "", nil}, {0.5, 0.2, SegmentStraight, nil}, {1, 1, SegmentGuided, &CurveGuide{X: 1, Y: 0.2, Pull: 2}}}},
		{name: "guide pull defaults to 1", in: []string{"7:100 VIA 3.5:50%"},
			want: []CurvePoint{{0, 0, "", nil}, {1, 1, SegmentGuided, &CurveGuide{X: 0.5, Y: 0.5, Pull: 1}}}},
		{name: "guide and kind", in: []string{"7:100 step via 3:20"}, wantErr: "both \"step\" and a guide"},
		{name: "guide outside", in: []string{"3:30", "7:100 via 2:40"}, wantErr: "guide 2:40% is outside the stretch from day 3 to day 7"},
		{name: "guide above", in: []string{"3:30", "7:100 via 5:120"}, wantErr: "outside the stretch"},
		{name: "guide on day 0", in: []string{"0:0 via 0:0", "7:100"}, wantErr: "guides the point on day 0"},
		{name: "pull too strong", in: []string{"7:100 via 3:20 pull 11"}, wantErr: "outside 0.1 to 10"},
		{name: "empty", in: nil, wantErr: "names no points"},
		{name: "garbage", in: []string{"day five"}, wantErr: "not day:percent"},
		{name: "unknown kind", in: []string{"7:100 wobbly"}, wantErr: "not smooth, straight or step"},
		{name: "past the week", in: []string{"8:100"}, wantErr: "past day 7"},
		{name: "too high", in: []string{"7:401"}, wantErr: "above 400%"},
		{name: "falls", in: []string{"3:50", "5:40"}, wantErr: "falls below"},
		{name: "same day", in: []string{"3:50", "3:60"}, wantErr: "does not come after"},
		{name: "starts above zero", in: []string{"0:10", "7:100"}, wantErr: "starts above 0%"},
		{name: "too many", in: strings.Fields("0.5:1 1:2 1.5:3 2:4 2.5:5 3:6 3.5:7 4:8 4.5:9 5:10 5.5:11 6:12 6.5:13 6.6:14 6.7:15 6.8:16"),
			wantErr: "more than 16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePoints(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParsePoints(%q) err = %v, want %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParsePoints(%q) = %+v, %v, want %+v", tc.in, got, err, tc.want)
			}
		})
	}
}

// A points curve the plugin cannot read runs the default curve and says so,
// one it can is parsed into Curve, and another shape drops it.
func TestNormalizePoints(t *testing.T) {
	c := Defaults()
	c.Pace.Shape, c.Pace.Points = ShapePoints, []string{"3:50", "2:60"}
	warnings := c.Normalize()
	if c.Pace.Shape != ShapeLinear || c.Pace.Curve != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "pace.points") {
		t.Errorf("unreadable points: shape %q curve %v warnings %q, want linear, no curve, one pace.points warning", c.Pace.Shape, c.Pace.Curve, warnings)
	}
	c = Defaults()
	c.Pace.Shape, c.Pace.Points = "Points", []string{"5:100", "7:120 straight"}
	if w := c.Normalize(); len(w) != 0 || c.Pace.Shape != ShapePoints || len(c.Pace.Curve) != 3 {
		t.Errorf("readable points: shape %q curve %v warnings %q, want the points shape", c.Pace.Shape, c.Pace.Curve, w)
	}
	c.Pace.Shape = ShapeLinear
	c.Normalize()
	if c.Pace.Curve != nil {
		t.Errorf("linear shape kept curve %v", c.Pace.Curve)
	}
}
