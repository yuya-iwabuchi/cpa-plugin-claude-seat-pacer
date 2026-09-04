package pace

import "testing"

func TestModelFamily(t *testing.T) {
	cases := []struct {
		modelID string
		want    string
	}{
		{"claude-opus-5", FamilyOpus},
		{"claude-opus-5[1m]", FamilyOpus},
		{"claude-sonnet-5", FamilySonnet},
		{"claude-fable-5-1", FamilyFable},
		{"claude-haiku-4-5-20251001", FamilyHaiku},
		{"anthropic/claude-opus-5", FamilyOpus},
		{"us.anthropic.claude-sonnet-5-v1:0", FamilySonnet},
		{"Claude-Opus-5", FamilyOpus},
		{"claude-3-5-haiku-latest", FamilyHaiku},
		{"gpt-5", ""},
		{"claude-5", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.modelID, func(t *testing.T) {
			if got := ModelFamily(tc.modelID); got != tc.want {
				t.Fatalf("ModelFamily(%q) = %q, want %q", tc.modelID, got, tc.want)
			}
		})
	}
}
