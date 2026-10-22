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
	tolerance := regexp.MustCompile(`\n  var RESET_TOLERANCE_MS = [^;\n]+;`).Find(indexHTML)
	if tolerance == nil {
		t.Fatal("page has no RESET_TOLERANCE_MS")
	}
	src := "var H_HOUR = 3600000, H_DAY = 86400000;" + string(tolerance) + "\n" +
		"function num(v) { return typeof v === \"number\" && isFinite(v); }\n" +
		pageFunctions(t, "bucketStart", "bucketAfter", "levelSteps", "stepAt", "stackSeries", "returnSchedule",
			"poolSize", "mergeSpans", "intersectSpans", "spanTime", "histPresets", "histAhead", "levelStart",
			"parseT", "estimatedAt", "fullRuns", "cutFull", "windowLocks", "settledStart", "settleSpans", "stackLocks", "rosterOff") + `
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

// Every span leads by one share of itself.
eq("leads", histPresets(true).concat(histPresets(false)).filter(function (p) { return p[1] !== Infinity; })
  .map(function (p) { return Math.round(histAhead(p[1]) / p[1] * 100); }), [15, 15, 15, 15, 15, 15, 15, 15, 15, 15]);

// A line comes to its present level at the riser into it, at its first
// reading, or after a clearing, and nowhere on a line at another level.
var at = function (t) { return t; }, pct = function (u) { return u * 100; };
var climb = [{ t: 0, u: 0.2 }, { t: 10, u: 0.2 }, { t: 10, u: 0.5 }, { t: 20, u: 0.5 }, { t: 20, u: 0.5 }, { t: 25, u: 0.5 }, { t: 25, u: 0.5 }];
var flat = [{ t: 0, u: 0.5 }, { t: 10, u: 0.5 }, { t: 10, u: 0.5 }, { t: 25, u: 0.5 }];
var cleared = [{ t: 0, u: 0.5 }, { t: 10, u: 0.5, fresh: true }, { t: 25, u: 0.5 }];
// A rise under half a pixel does not end a run, and one of half a pixel or
// more does.
var crept = [{ t: 0, u: 0.5 }, { t: 10, u: 0.5 }, { t: 10, u: 0.502 }, { t: 25, u: 0.502 }];
var stepped = [{ t: 0, u: 0.5 }, { t: 10, u: 0.5 }, { t: 10, u: 0.507 }, { t: 25, u: 0.507 }];
// A run at the level that ends before x does not reach it.
var dipped = [{ t: 0, u: 0.5 }, { t: 10, u: 0.5 }, { t: 10, u: 0.2, fresh: true }, { t: 20, u: 0.2 }, { t: 20, u: 0.5 }, { t: 25, u: 0.5 }];
eq("level start", [levelStart(climb, 25, 50, at, pct), levelStart(flat, 25, 50, at, pct), levelStart(cleared, 25, 50, at, pct),
  levelStart(climb, 25, 20, at, pct), levelStart(crept, 25, 50.2, at, pct), levelStart(stepped, 25, 50.7, at, pct), levelStart(dipped, 25, 50, at, pct),
  levelStart([], 25, 50, at, pct)],
  [10, 0, 10, 25, 0, 10, 20, 25]);

// A window reads full from its first reading at full to the first under it,
// to an early clearing the next cycle opens with, or to its reset; an
// estimate is no reading, and a cycle with no reset gives nothing.
var T0 = Date.parse("2026-10-20T00:00:00Z") / 1000, hr = 3600;
var iso = function (s) { return new Date(s * 1000).toISOString(); };
var cyc = function (reset, samples, more) { return Object.assign({ resets_at: iso(T0 + reset), samples: samples.map(function (s) { return [T0 + s[0], s[1]]; }) }, more || {}); };
var ms = function (h) { return (T0 + h * hr) * 1000; };
eq("full to the reset", fullRuns({ cycles: [cyc(10 * hr, [[0, 0.5], [2 * hr, 1], [3 * hr, 1]])] }), [{ from: ms(2), to: ms(10), end: "reset" }]);
eq("full to a clearing", fullRuns({ cycles: [cyc(10 * hr, [[hr, 1]]), cyc(10 * hr, [[4 * hr, 0.1]])] }), [{ from: ms(1), to: ms(4), end: "cleared" }]);
// A next cycle opening within the tolerance of the reset is the reset, and a
// full reading at or past its cycle's end draws nothing.
eq("next cycle at the reset", fullRuns({ cycles: [cyc(10 * hr, [[hr, 1]]), cyc(17 * 24 * hr, [[10 * hr - 60, 0.1]])] }), [{ from: ms(1), to: ms(10), end: "reset" }]);
eq("full at the cycle's end", fullRuns({ cycles: [cyc(10 * hr, [[10 * hr, 1]])] }), []);
eq("full to a lower reading", fullRuns({ cycles: [cyc(10 * hr, [[hr, 1], [2 * hr, 0.9]])] }), [{ from: ms(1), to: ms(2), end: "fell" }]);
eq("an estimate is no reading", [fullRuns({ cycles: [cyc(10 * hr, [[hr, 1]], { estimated: true })] }),
  fullRuns({ cycles: [cyc(10 * hr, [[hr, 1], [3 * hr, 1]], { estimated_until: iso(T0 + 2 * hr) })] })],
  [[], [{ from: ms(3), to: ms(10), end: "reset" }]]);
eq("no reset", fullRuns({ cycles: [{ samples: [[T0 + hr, 1]] }] }), []);
// A run reaching its reset is ongoing until then; a refusal cuts it, the
// piece before ending as refused; a piece under the tolerance is dropped.
var run = [{ from: ms(1), to: ms(10), end: "reset" }];
eq("ongoing and past", [cutFull(run, [], ms(5)), cutFull(run, [], ms(11))],
  [[{ from: ms(1), to: ms(10), ended: "", full: true }], [{ from: ms(1), to: ms(10), ended: "reset", full: true }]]);
eq("cut by a refusal", cutFull(run, [{ from: ms(3), to: ms(5), ended: "served" }], ms(11)),
  [{ from: ms(1), to: ms(3), ended: "refused", full: true }, { from: ms(5), to: ms(10), ended: "reset", full: true }]);
eq("moments before a refusal", cutFull(run, [{ from: ms(1) + 60000, to: ms(10), ended: "reset" }], ms(11)), []);
// Full stretches keep their start, and so does a refusal a kept full piece
// runs into; a refusal alone, or one a dropped piece runs into, settles.
var lock = function (from, to, end) { return { from: (T0 + from * hr), to: (T0 + to * hr), end: end }; };
var starts = function (locks) { return locks.map(function (l) { return [l.full, l.keepStart]; }); };
eq("kept starts", starts(windowLocks({ locks: [lock(3, 5, "served")] }, run, ms(11))), [[false, true], [true, true], [true, true]]);
eq("settling starts", [starts(windowLocks({ locks: [lock(3, 5, "served")] }, [], ms(11))),
  starts(windowLocks({ locks: [lock(1 + 1 / 60, 10, "reset")] }, run, ms(11)))], [[[false, false]], [[false, false]]]);
// A refusal starts on the line at the first reading after it that rose, use
// from before it caught late; a span marked keepStart keeps its start.
var settled = [{ t0: 5, t1: 20 }, { t0: 5, t1: 20, keepStart: true }];
settleSpans(settled, [{ t: 0, u: 0.5 }, { t: 10, u: 0.6 }, { t: 20, u: 0.6 }], 20, 0.6);
eq("settled starts", settled.map(function (sp) { return sp.t0; }), [10, 5]);
// Stacked merges refusals before settling them, so two that overlap strike
// one unbroken stretch from the first rise, and draws a span marked
// keepStart from its own start.
var win = [{ start: 0, end: 40, samples: [{ t: 0, u: 0.5 }, { t: 25, u: 0.6 }, { t: 40, u: 0.6 }], endU: 0.6 }];
eq("stacked refusals", stackLocks([{ t0: 10, t1: 20 }, { t0: 15, t1: 30 }], win), [[25, 30]]);
eq("stacked kept start", stackLocks([{ t0: 10, t1: 30, keepStart: true }, { t0: 32, t1: 38 }], win), [[10, 30], [32, 38]]);
// A seat is off the roster to the reset a 7d Standard lock expects; an
// ongoing full stretch runs past now only while the snapshot reads full.
// A refusal, or a full stretch that has ended, keeps its own end.
var off = [{ kind: "seven_day", from: 10, to: 100, ended: "", full: true }, { kind: "five_hour", from: 0, to: 5, ended: "reset" },
  { kind: "seven_day", from: 0, to: 8, ended: "fell", full: true }, { kind: "seven_day", from: 60, to: 120, ended: "" }];
eq("roster off", [rosterOff(off, true, 50), rosterOff(off, false, 50)], [[[0, 8], [10, 120]], [[0, 8], [10, 50], [60, 120]]]);

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
