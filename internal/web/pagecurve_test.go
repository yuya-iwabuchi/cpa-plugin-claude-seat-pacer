package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/pace"
)

// TestPageCurveMatchesTheScorer runs the page's target curve under node and
// holds it to pace.Target for every shape, landings either side of full, and
// the curve held at full and not, so the line the charts draw is the one the
// pick is scored against.
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
	want, err := json.Marshal(points)
	if err != nil {
		t.Fatal(err)
	}
	src := "function num(v) { return typeof v === \"number\" && isFinite(v); }\n" +
		"function clamp(v, lo, hi) { return v < lo ? lo : (v > hi ? hi : v); }\n" +
		pageFunctions(t, "landingOf", "shapeAt", "targetAt", "curveTop", "fullAt") + `
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
