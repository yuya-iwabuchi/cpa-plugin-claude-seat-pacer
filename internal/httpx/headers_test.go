package httpx

import "testing"

func TestNewIndexFoldsCaseAndTakesTheFirstNonEmptyValue(t *testing.T) {
	t.Parallel()
	idx := NewIndex(map[string][]string{
		"X-Thing": {"", "  ", " first ", "second"},
		"Blank":   {"", "   "},
	})
	if got := idx.Get("x-thing"); got != "first" {
		t.Fatalf("Get(x-thing) = %q, want %q", got, "first")
	}
	if got := idx.Get("X-THING"); got != "first" {
		t.Fatalf("Get(X-THING) = %q, want %q", got, "first")
	}
	if _, ok := idx.Lookup("blank"); ok {
		t.Fatal("a header whose every value is blank reads as present")
	}
	if _, ok := idx.Lookup("absent"); ok {
		t.Fatal("an absent header reads as present")
	}
	if got := idx.Get("absent"); got != "" {
		t.Fatalf("Get(absent) = %q, want empty", got)
	}
}

// Two capitalizations of one name collapse to one entry, and the spelling
// that sorts first supplies it whatever order the map range visits them in.
func TestNewIndexIsDeterministicAcrossCasings(t *testing.T) {
	t.Parallel()
	headers := map[string][]string{
		"Session-Id": {"upper"},
		"session-id": {"lower"},
		"SESSION-ID": {"shout"},
	}
	for i := 0; i < 200; i++ {
		if got := NewIndex(headers).Get("session-id"); got != "shout" {
			t.Fatalf("iteration %d: Get = %q, want %q", i, got, "shout")
		}
	}
}

func TestNewIndexOfNilIsEmpty(t *testing.T) {
	t.Parallel()
	idx := NewIndex(nil)
	if len(idx) != 0 {
		t.Fatalf("len = %d, want 0", len(idx))
	}
	if got := idx.Get("anything"); got != "" {
		t.Fatalf("Get on empty index = %q", got)
	}
}
