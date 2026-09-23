package main

import (
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// TestHostCallbacksReadStoredHostOnce holds the two C helpers to a single read
// of stored_host. The pointer is a plain global that cliproxy_plugin_shutdown
// nulls once Shutdown's bounded drain has given up, so a helper that
// dereferences it again after its own NULL check can fault in unmapped memory
// — a SIGSEGV no Go panic guard can catch.
func TestHostCallbacksReadStoredHostOnce(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"call_host_api", "free_host_buffer"} {
		body := cHelperBody(t, string(src), fn)
		if strings.Contains(body, "stored_host->") {
			t.Errorf("%s dereferences stored_host directly:\n%s", fn, body)
		}
		if strings.Count(body, "stored_host") != 1 {
			t.Errorf("%s reads stored_host %d times, want one load into a local:\n%s",
				fn, strings.Count(body, "stored_host"), body)
		}
	}
}

// cHelperBody is the text of one static C function of the cgo preamble, from
// its signature to the closing brace in the first column.
func cHelperBody(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "static int "+name+"(")
	if i < 0 {
		i = strings.Index(src, "static void "+name+"(")
	}
	if i < 0 {
		t.Fatalf("no C helper named %s", name)
	}
	rest := src[i:]
	j := strings.Index(rest, "\n}\n")
	if j < 0 {
		t.Fatalf("%s has no closing brace", name)
	}
	return rest[:j+2]
}

// TestBufferTooLargeGuardsGoBytes holds the bound to what C.GoBytes takes: its
// length is a C int, so a size_t past MaxInt32 wraps negative and
// runtime.gobytes throws a fatal error that no recover in this file catches.
func TestBufferTooLargeGuardsGoBytes(t *testing.T) {
	for _, tc := range []struct {
		n    uint64
		want bool
	}{
		{0, false},
		{1 << 20, false},
		{math.MaxInt32 - 1, false},
		{math.MaxInt32, false},
		{math.MaxInt32 + 1, true},
		{1 << 33, true},
		{math.MaxUint64, true},
	} {
		if got := bufferTooLarge(tc.n); got != tc.want {
			t.Errorf("bufferTooLarge(%d) = %v, want %v", tc.n, got, tc.want)
		}
		if !tc.want && int64(int32(tc.n)) != int64(tc.n) {
			t.Errorf("bufferTooLarge(%d) admits a length that truncates to %d", tc.n, int32(tc.n))
		}
	}
}

// The host renders its own enable toggle for every plugin and loads no
// disabled plugin, so an enabled field would be a second switch for one state.
func TestConfigFieldsLeaveEnabledToTheHost(t *testing.T) {
	for _, field := range configFields {
		if field.Name == "enabled" {
			t.Errorf("configFields declares %q, which the host's own toggle owns", field.Name)
		}
	}
}

// pace.shape takes one of three names, so the Management Center offers them
// as a choice rather than a free string that falls back on a typo.
func TestPaceShapeIsAChoiceOfTheThreeShapes(t *testing.T) {
	for _, field := range configFields {
		if field.Name != "pace.shape" {
			continue
		}
		want := []string{model.ShapeLinear, model.ShapePower, model.ShapeSigmoid}
		if field.Type != "enum" || !slices.Equal(field.EnumValues, want) {
			t.Errorf("pace.shape = %s %v, want enum %v", field.Type, field.EnumValues, want)
		}
		return
	}
	t.Error("configFields declares no pace.shape")
}
