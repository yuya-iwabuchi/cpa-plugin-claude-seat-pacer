package model

import "testing"

// TestScopedWindowOfAnUnknownFamilyBearsOnNoFamilylessModel covers a scoped cap
// the endpoint names for a family this build does not know: its scope maps to
// no family, as a model with no family does, and the two must not match.
func TestScopedWindowOfAnUnknownFamilyBearsOnNoFamilylessModel(t *testing.T) {
	w := Window{Kind: WindowWeeklyScoped, Scope: "Claude Nova 1"}
	if FamilyOf(w.Scope) != "" {
		t.Fatalf("FamilyOf(%q) = %q, want no known family", w.Scope, FamilyOf(w.Scope))
	}
	if w.BearsOn("") {
		t.Error("a scoped cap of an unknown family bears on a model with no family")
	}
}
