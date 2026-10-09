package web

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pageFunctions returns the source of each named top-level function of the
// page's script, which the script indents by two spaces and closes with a
// brace at that indent.
func pageFunctions(t *testing.T, names ...string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range names {
		start := regexp.MustCompile(`\n  function ` + name + `\(`).FindIndex(indexHTML)
		if start == nil {
			t.Fatalf("page has no function %s", name)
		}
		end := bytes.Index(indexHTML[start[0]:], []byte("\n  }\n"))
		if end < 0 {
			t.Fatalf("function %s has no end", name)
		}
		b.Write(indexHTML[start[0] : start[0]+end+5])
	}
	return b.String()
}

// TestPageMath runs the page's pure time and quota arithmetic under node
// against known answers, once in each of two zones: one whose
// daylight-saving changes keep midnight, and one whose spring change skips
// it.
func TestPageMath(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src := "var H_HOUR = 3600000, H_DAY = 86400000;\n" +
		"function num(v) { return typeof v === \"number\" && isFinite(v); }\n" +
		pageFunctions(t, "bucketStart", "bucketAfter", "levelSteps", "stepAt", "stackSeries", "returnSchedule",
			"poolSize", "mergeSpans", "intersectSpans", "spanTime", "histPresets", "histAhead") + `
var fails = [];
function eq(name, got, want) {
  if (JSON.stringify(got) !== JSON.stringify(want)) fails.push(name + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want));
}
var local = function (y, mo, d, h, mi) { return new Date(y, mo - 1, d, h, mi || 0).getTime(); };

// A day opens at midnight, a week on Monday at midnight.
eq("day of 3 AM", bucketStart(local(2026, 9, 30, 3), "day"), local(2026, 9, 30, 0));
eq("day of 11 PM", bucketStart(local(2026, 9, 30, 23), "day"), local(2026, 9, 30, 0));
eq("week of Sun", bucketStart(local(2026, 10, 4, 23), "week"), local(2026, 9, 28, 0));
eq("week of Mon", bucketStart(local(2026, 10, 5, 0, 30), "week"), local(2026, 10, 5, 0));
// Days and weeks tile the year: each bucket ends where the next starts, and
// stepping back from a bucket's start lands on the bucket before it.
["day", "week"].forEach(function (unit) {
  for (var b = bucketStart(local(2026, 1, 1, 12), unit), n = 0; b < local(2027, 1, 1, 0); b = bucketAfter(b, unit), n++) {
    var next = bucketAfter(b, unit);
    if (bucketStart(next, unit) !== next || bucketStart(next - 1, unit) !== b || bucketAfter(bucketStart(b - 1, unit), unit) !== b) {
      fails.push(unit + " bucket at " + new Date(b) + " does not tile");
      break;
    }
  }
});
if (process.env.TZ === "America/New_York") {
  // The day of the fall-back change is 25 hours and still ends at midnight.
  var fb = local(2026, 11, 1, 0);
  eq("DST day length", (bucketAfter(fb, "day") - fb) / H_HOUR, 25);
  eq("DST next day", bucketAfter(fb, "day"), local(2026, 11, 2, 0));
}
if (process.env.TZ === "America/Santiago") {
  // Sep 6 2026 has no midnight: it opens at 1 AM and is 23 hours, and its
  // week still opens at the Monday's midnight.
  var sk = local(2026, 9, 6, 12);
  eq("skipped midnight day", bucketStart(sk, "day"), local(2026, 9, 6, 1));
  eq("skipped midnight day length", (bucketAfter(bucketStart(sk, "day"), "day") - bucketStart(sk, "day")) / H_HOUR, 23);
  eq("skipped midnight day before", bucketAfter(bucketStart(sk - H_DAY, "day"), "day"), local(2026, 9, 6, 1));
  eq("skipped midnight week", [bucketStart(sk, "week"), bucketAfter(bucketStart(sk, "week"), "week")], [local(2026, 8, 31, 0), local(2026, 9, 7, 0)]);
  eq("week before skipped midnight", bucketStart(local(2026, 9, 7, 0) - 1, "week"), local(2026, 8, 31, 0));
}

// Spans merge where they meet and intersect to their overlap.
var m = mergeSpans([[5, 8], [0, 2], [1, 3], [8, 9]]);
eq("merge", m, [[0, 3], [5, 9]]);
eq("intersect", intersectSpans(m, [[2, 6], [8.5, 20]]), [[2, 3], [5, 6], [8.5, 9]]);
eq("span time", spanTime(m, 1, 6), 3);

// A level holds each reading, zero between windows, the present reading
// to the reset, and zero after it.
var wins = [{ samples: [{ t: 0, u: 0.2 }, { t: 10, u: 0.5 }], end: 20 }, { samples: [{ t: 30, u: 0.1 }] }];
var steps = levelSteps(wins, 0.4, 100, 50);
eq("steps", steps, [{ t: 0, u: 0.2 }, { t: 10, u: 0.5 }, { t: 20, u: 0 }, { t: 30, u: 0.1 }, { t: 50, u: 0.4 }, { t: 100, u: 0 }]);
eq("level before", stepAt(steps, -1), null);
eq("level between", stepAt(steps, 25), 0);
eq("level held", stepAt(steps, 99), 0.4);
eq("level after reset", stepAt(steps, 120), 0);
// With no present reading the level falls at the last window's end; with
// no history it starts at now.
eq("no reading", levelSteps([{ samples: [{ t: 0, u: 0.3 }], end: 40 }], null, null, 50), [{ t: 0, u: 0.3 }, { t: 40, u: 0 }]);
eq("no history", levelSteps([], 0.2, 90, 50), [{ t: 50, u: 0.2 }, { t: 90, u: 0 }]);
// A present reading whose reset has passed falls to zero at that reset.
eq("reset passed", levelSteps([{ samples: [{ t: 0, u: 0.3 }, { t: 10, u: 0.4 }], end: 50 }], 0.4, 30, 50),
  [{ t: 0, u: 0.3 }, { t: 10, u: 0.4 }, { t: 30, u: 0 }]);
eq("reset passed, no history", levelSteps([], 0.2, 40, 50), [{ t: 50, u: 0 }]);

// A stack sums its lists at every breakpoint.
var st = stackSeries([[{ t: 0, u: 0.5 }], [{ t: 5, u: 1 }, { t: 8, u: 0 }]], 0, 10, [7]);
eq("stack ts", st.ts, [0, 5, 7, 8, 10]);
eq("stack top", st.tops[1], [0.5, 1.5, 1.5, 0.5, 0.5]);

// Only seats holding use with a reset ahead come back, soonest first.
var uses = [
  { cur: 0.3, resets: new Date(300), first: 0, a: "b" },
  { cur: 1, resets: new Date(200), first: 50, a: "a" },
  { cur: 0, resets: new Date(150), first: 0, a: "idle" },
  { cur: 0.5, resets: new Date(90), first: 0, a: "past" }
];
eq("returns", returnSchedule(uses, 100).map(function (r) { return r.a; }), ["a", "b"]);
eq("pool size", [poolSize(uses, 10, 100), poolSize(uses, 60, 100)], [3, 4]);
// A seat read now with no history is in the pool from now.
eq("pool size of a new seat", [poolSize([{ first: null, cur: 0.2 }], 50, 100), poolSize([{ first: null, cur: 0.2 }], 100, 100)], [0, 1]);

// Each preset leads by its own amount; spans between interpolate.
var p = histPresets(true);
eq("leads", [H_DAY, 7 * H_DAY, 30 * H_DAY, 60 * H_DAY].map(function (s) { return histAhead(p, s) / H_HOUR; }), [2, 12, 48, 48]);
eq("lead between", histAhead(p, 5 * H_DAY) / H_HOUR, 9);
var q = histPresets(false);
eq("5h leads", [6 * H_HOUR, 3 * H_DAY, 7 * H_DAY].map(function (s) { return histAhead(q, s) / H_HOUR; }), [0.5, 5, 5]);

if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
`
	file := filepath.Join(t.TempDir(), "math.js")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	// node reads TZ once at startup, so each zone takes its own run.
	for _, zone := range []string{"America/New_York", "America/Santiago"} {
		cmd := exec.Command(node, file)
		cmd.Env = append(os.Environ(), "TZ="+zone)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("page arithmetic in %s: %v\n%s", zone, err, out)
		}
	}
}
