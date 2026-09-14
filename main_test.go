package main

import (
	"os"
	"strings"
	"testing"
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
