package pace

import "strings"

// Model family names as the usage endpoint spells them in
// scope.model.display_name, which is the value a model-family weekly window
// carries in its Scope.
const (
	FamilyOpus   = "Opus"
	FamilySonnet = "Sonnet"
	FamilyFable  = "Fable"
	FamilyHaiku  = "Haiku"
)

// families maps the token a model id carries to the family it names. The order
// is fixed so an id containing two tokens always resolves the same way.
var families = []struct {
	token  string
	family string
}{
	{"opus", FamilyOpus},
	{"sonnet", FamilySonnet},
	{"fable", FamilyFable},
	{"haiku", FamilyHaiku},
}

// ModelFamily is the scoped-window family a model id belongs to. Matching is a
// case-insensitive substring test on the family token, so provider prefixes,
// date suffixes and context-window markers all resolve: claude-opus-5,
// claude-opus-5[1m], anthropic/claude-sonnet-5 and claude-haiku-4-5-20251001
// each name their family.
//
// An id naming no known family returns "". Scoring then has no scoped term:
// every scoped window belongs to some other family and is ignored, leaving the
// session and weekly terms to decide.
func ModelFamily(modelID string) string {
	lower := strings.ToLower(modelID)
	for _, f := range families {
		if strings.Contains(lower, f.token) {
			return f.family
		}
	}
	return ""
}
