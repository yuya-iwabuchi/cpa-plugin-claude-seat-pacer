package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/pace"
)

// TestPageCurveMatchesTheScorer runs the page's target curve under node and
// holds it to pace.Target for every shape, landings either side of full, the
// curve held at full and not, and points curves mixing every segment kind, so
// the line the charts draw is the one the pick is scored against.
func TestPageCurveMatchesTheScorer(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	type point struct {
		Cfg     map[string]any `json:"cfg"`
		Elapsed float64        `json:"e"`
		Want    float64        `json:"want"`
	}
	var points []point
	for _, shape := range []string{model.ShapeLinear, model.ShapePower, model.ShapeSigmoid} {
		for _, landing := range []float64{0.9, 1, 1.1, 2} {
			for _, hold := range []bool{false, true} {
				cfg := model.Defaults().Pace
				cfg.Shape, cfg.LandingTarget, cfg.HoldAtFull, cfg.CurveExponent = shape, landing, hold, 2
				js := map[string]any{"shape": shape, "landing_target": landing, "hold_at_full": hold,
					"curve_exponent": cfg.CurveExponent, "steepness": cfg.Steepness}
				for step := 0; step <= 20; step++ {
					e := float64(step) / 20
					points = append(points, point{js, e, pace.Target(cfg, e)})
				}
			}
		}
	}
	for _, entries := range [][]string{
		{"0:0", "3.5:30", "5:100 smooth", "6:100 straight", "7:150 step"},
		{"0.5:40", "1:41", "4:42", "4.2:100", "7:101"},
		{"5:100 straight"},
		{"2:10 step", "4:60", "6.36:100", "7:110 straight"},
		{"2:10 via 2:0 pull 10", "4:90 via 2:90 pull 0.1", "5:95", "7:120 via 5:120 pull 3"},
		{"3.5:30 via 3.5:0", "7:100 via 4:30 pull 2.5"},
	} {
		cfg := model.Defaults()
		cfg.Pace.Shape, cfg.Pace.Points = model.ShapePoints, entries
		if w := cfg.Normalize(); len(w) != 0 {
			t.Fatalf("Normalize(%q) warned %q", entries, w)
		}
		js := map[string]any{"shape": model.ShapePoints, "curve": cfg.Pace.Curve, "landing_target": 1.1, "hold_at_full": true}
		for step := 0; step <= 70; step++ {
			e := float64(step) / 70
			points = append(points, point{js, e, pace.Target(cfg.Pace, e)})
		}
	}
	want, err := json.Marshal(points)
	if err != nil {
		t.Fatal(err)
	}
	src := "function num(v) { return typeof v === \"number\" && isFinite(v); }\n" +
		"function clamp(v, lo, hi) { return v < lo ? lo : (v > hi ? hi : v); }\n" +
		pageFunctions(t, "landingOf", "shapeAt", "pointsAt", "guidedAt", "curveTangents", "rawTarget", "targetAt", "curveTop", "fullAt") + `
var fails = [];
` + "var points = " + string(want) + `;
points.forEach(function (p) {
  var got = targetAt(p.cfg, p.e);
  if (Math.abs(got - p.want) > 1e-9) fails.push(JSON.stringify(p.cfg) + " at " + p.e + ": got " + got + ", want " + p.want);
  if (p.e === 1 && Math.abs(curveTop(p.cfg) - p.want) > 1e-9) fails.push(JSON.stringify(p.cfg) + ": curveTop " + curveTop(p.cfg) + ", want " + p.want);
  var full = fullAt(p.cfg);
  if (full !== null && Math.abs(Math.min(targetAt(p.cfg, full), 1) - 1) > 1e-6) fails.push(JSON.stringify(p.cfg) + ": fullAt " + full + " is not where the curve reaches full");
});
if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
`
	file := filepath.Join(t.TempDir(), "curve.js")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil {
		t.Errorf("page curve: %v\n%s", err, out)
	}
}

// TestPageTuneSaveBody runs the page's tuning save body under node: changes go
// into a nested pace mapping where the block has one, removing a dotted twin,
// and are dotted keys where it has none.
func TestPageTuneSaveBody(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := pageFunctions(t, "tuneSaveBody") + `
var fails = [];
function eq(name, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) fails.push(name + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want));
}
var changed = [["landing_target", "pace.landing-target"], ["hold_at_full", "pace.hold-at-full"]];
var draft = { landing_target: 1.3, hold_at_full: true, shape: "linear" };
eq("no block", tuneSaveBody(null, changed, draft), { "pace.landing-target": 1.3, "pace.hold-at-full": true });
eq("flat block", tuneSaveBody({ enabled: true, "pace.shape": "power" }, changed, draft), { "pace.landing-target": 1.3, "pace.hold-at-full": true });
var block = { enabled: true, pace: { shape: "sigmoid", "landing-target": 1.1 }, "pace.landing-target": 1.2 };
eq("nested block", tuneSaveBody(block, changed, draft),
  { "pace.landing-target": null, pace: { shape: "sigmoid", "landing-target": 1.3, "hold-at-full": true } });
eq("nested block untouched", block.pace, { shape: "sigmoid", "landing-target": 1.1 });
if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
`
	file := filepath.Join(t.TempDir(), "tune.js")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil {
		t.Errorf("tune save body: %v\n%s", err, out)
	}
}

// TestPagePointsPlanParses runs the page's points plan under node and holds
// the curve it draws to the one model.ParsePoints reads from the entries it
// writes, points that rounding brings together included, so a saved curve
// runs as it was drawn.
func TestPagePointsPlanParses(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := "var CURVE_DAYS = 7;\n" + pageFunctions(t, "pointDay", "pointPct", "pointsPlan") + `
var plans = [
  [{ x: 0, y: 0, kind: "" }, { x: 0.3, y: 0.2, kind: "smooth" }, { x: 6.36 / 7, y: 1, kind: "straight" }, { x: 1, y: 1.10003, kind: "step" }],
  [{ x: 0, y: 0, kind: "" }, { x: 1 / 3, y: 0.123456, kind: "step" }, { x: 1, y: 2.5, kind: "smooth" }],
  // Rounding puts these on one day, and the second below the first.
  [{ x: 0, y: 0, kind: "" }, { x: 1.001 / 7, y: 0.3, kind: "smooth" }, { x: 1.004 / 7, y: 0.29996, kind: "straight" }, { x: 6.999 / 7, y: 1, kind: "straight" }, { x: 1, y: 1, kind: "straight" }],
  // Guides that round outside their stretch, and one whose stretch's start rounds away.
  [{ x: 0, y: 0, kind: "" }, { x: 2 / 7, y: 0.2, kind: "guided", guide: { x: 2.004 / 7, y: 0.00001, pull: 1.234 } },
   { x: 5 / 7, y: 0.9, kind: "guided", guide: { x: 1.996 / 7, y: 0.95, pull: 1 } }, { x: 1, y: 1.3, kind: "guided", guide: { x: 6 / 7, y: 1, pull: 12 } }],
  [{ x: 0, y: 0, kind: "" }, { x: 1.001 / 7, y: 0.3, kind: "smooth" }, { x: 1.004 / 7, y: 0.4, kind: "guided", guide: { x: 1.002 / 7, y: 0.35, pull: 0.5 } }, { x: 1, y: 1, kind: "guided", guide: { x: 0.5 / 7, y: 0.2, pull: 1 } }]
].map(pointsPlan);
console.log(JSON.stringify(plans));
`
	file := filepath.Join(t.TempDir(), "plan.js")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).CombinedOutput()
	if err != nil {
		t.Fatalf("points plan: %v\n%s", err, out)
	}
	var plans []struct {
		Points []string           `json:"points"`
		Curve  []model.CurvePoint `json:"curve"`
	}
	if err := json.Unmarshal(out, &plans); err != nil {
		t.Fatalf("points plan output %s: %v", out, err)
	}
	for _, p := range plans {
		got, err := model.ParsePoints(p.Points)
		if err != nil {
			t.Errorf("ParsePoints(%q): %v", p.Points, err)
			continue
		}
		if !reflect.DeepEqual(got, p.Curve) {
			t.Errorf("ParsePoints(%q) = %+v, page draws %+v", p.Points, got, p.Curve)
		}
	}
}
