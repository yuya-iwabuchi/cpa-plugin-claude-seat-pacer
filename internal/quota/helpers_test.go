package quota

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// testNow anchors every fixture's reset instants, so a reading's placement
// inside its window is deterministic.
var testNow = time.Date(2026, 9, 4, 17, 30, 0, 0, time.UTC)

// utilizationEpsilon is loose enough to survive float rounding and far tighter
// than the factor-of-100 scaling mistake the tests exist to catch.
const utilizationEpsilon = 1e-12

func at(hour, minute int) time.Time {
	return time.Date(2026, 9, 4, hour, minute, 0, 0, time.UTC)
}

func day(d, hour int) time.Time {
	return time.Date(2026, 9, d, hour, 0, 0, 0, time.UTC)
}

func epoch(hour, minute int) string {
	return strconv.FormatInt(at(hour, minute).Unix(), 10)
}

func epochMillis(hour, minute int) string {
	return strconv.FormatInt(at(hour, minute).UnixMilli(), 10)
}

func epochDay(d, hour int) string {
	return strconv.FormatInt(day(d, hour).Unix(), 10)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// assertWindows compares readings field by field, using Time.Equal so a
// difference in a Time's internal representation does not read as a failure.
func assertWindows(t *testing.T, got, want []model.Window) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("window count = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Kind != w.Kind || g.Scope != w.Scope {
			t.Errorf("window %d identity = (%s,%q), want (%s,%q)", i, g.Kind, g.Scope, w.Kind, w.Scope)
		}
		if math.Abs(g.Utilization-w.Utilization) > utilizationEpsilon {
			t.Errorf("window %d %s utilization = %v, want %v", i, g.Kind, g.Utilization, w.Utilization)
		}
		if !g.ResetsAt.Equal(w.ResetsAt) {
			t.Errorf("window %d %s resets_at = %v, want %v", i, g.Kind, g.ResetsAt, w.ResetsAt)
		}
		if g.Duration != w.Duration {
			t.Errorf("window %d %s duration = %v, want %v", i, g.Kind, g.Duration, w.Duration)
		}
		if g.Status != w.Status {
			t.Errorf("window %d %s status = %q, want %q", i, g.Kind, g.Status, w.Status)
		}
		if g.Severity != w.Severity {
			t.Errorf("window %d %s severity = %q, want %q", i, g.Kind, g.Severity, w.Severity)
		}
		if g.Active != w.Active {
			t.Errorf("window %d %s active = %v, want %v", i, g.Kind, g.Active, w.Active)
		}
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
