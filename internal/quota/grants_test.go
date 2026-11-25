package quota

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

func TestParseResetGrants(t *testing.T) {
	got := ParseResetGrants(fixture(t, "usage_reset_grants.json"))
	if got == nil {
		t.Fatal("ParseResetGrants = nil, want the grants block")
	}
	fullEnd := time.Date(2026, 9, 10, 22, 0, 0, 0, time.UTC)
	fableEnd := time.Date(2026, 12, 9, 7, 59, 0, 0, time.UTC)
	session := model.WindowRef{Kind: model.WindowSession}
	weekly := model.WindowRef{Kind: model.WindowWeekly}
	fable := model.WindowRef{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable}
	want := []model.ResetGrant{
		{ID: "full-reset-2609", Type: model.ResetTypeFull, Left: 1, EndsAt: &fullEnd,
			Clears: []model.WindowRef{session, weekly, fable}, UsableNow: true},
		{ID: "launch-2608-fable", Type: model.ResetTypeFull, Family: model.FamilyFable, Left: 1, EndsAt: &fableEnd,
			Clears: []model.WindowRef{session, fable}, UsableNow: true, GivesBack: true},
		{ID: "session-2609", Type: model.ResetTypeSession, Left: 0,
			Clears: []model.WindowRef{session}, Paused: true, Unavailable: "unavailable"},
	}
	if len(got.Grants) != len(want) {
		t.Fatalf("grants = %d, want %d: %+v", len(got.Grants), len(want), got.Grants)
	}
	for i := range want {
		g, w := got.Grants[i], want[i]
		if !timeEqual(g.EndsAt, w.EndsAt) {
			t.Errorf("grant %d ends_at = %v, want %v", i, g.EndsAt, w.EndsAt)
		}
		g.EndsAt, w.EndsAt = nil, nil
		if !reflect.DeepEqual(g, w) {
			t.Errorf("grant %d =\n%+v\nwant\n%+v", i, g, w)
		}
	}
}

func TestParseResetGrantsAbsent(t *testing.T) {
	for name, body := range map[string]string{
		"no block":   `{"five_hour": {"utilization": 1}}`,
		"null block": `{"cedar_ember": null}`,
		"malformed":  `{"cedar_ember": {"grants": "none"}}`,
		"not json":   `{`,
	} {
		if got := ParseResetGrants([]byte(body)); got != nil {
			t.Errorf("%s: ParseResetGrants = %+v, want nil", name, got)
		}
	}
}

func TestParseResetGrantsLeavesOutAGrantItCannotRead(t *testing.T) {
	got := ParseResetGrants([]byte(`{"cedar_ember": {"eligible": false, "cooldown_until": 5, "grants": [
		{"id": "bad", "resets_left": 1, "unavailable_reason": {"code": 1}},
		{"id": "good", "type": "full", "resets_left": 1, "clears": ["five_hour"]}]}}`))
	if got == nil || len(got.Grants) != 1 || got.Grants[0].ID != "good" {
		t.Errorf("ParseResetGrants = %+v, want only the grant it can read", got)
	}
	if got := ParseResetGrants([]byte(`{"cedar_ember": {"grants": null}}`)); got == nil || got.Grants == nil || len(got.Grants) != 0 {
		t.Errorf("ParseResetGrants with no grants = %+v, want an empty list", got)
	}
}

func TestGrantFamily(t *testing.T) {
	opus := model.WindowRef{Kind: model.WindowWeeklyScoped, Scope: model.FamilyOpus}
	fable := model.WindowRef{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable}
	cases := []struct {
		clears []model.WindowRef
		want   string
	}{
		{nil, ""},
		{[]model.WindowRef{{Kind: model.WindowSession}}, ""},
		{[]model.WindowRef{{Kind: model.WindowSession}, fable}, model.FamilyFable},
		{[]model.WindowRef{{Kind: model.WindowWeekly}, fable}, ""},
		{[]model.WindowRef{opus, fable}, ""},
	}
	for _, c := range cases {
		if got := grantFamily(c.clears); got != c.want {
			t.Errorf("grantFamily(%v) = %q, want %q", c.clears, got, c.want)
		}
	}
}

func TestWithGrantQuery(t *testing.T) {
	cases := map[string]string{
		"https://api.anthropic.com/api/oauth/usage":           "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1",
		"http://127.0.0.1:9/usage?x=y;z&b=%20#f":              "http://127.0.0.1:9/usage?x=y;z&b=%20&cedar_ember=1&skip_spend=1#f",
		"http://127.0.0.1:9/usage?skip_spend=0":               "http://127.0.0.1:9/usage?skip_spend=0&cedar_ember=1",
		"http://127.0.0.1:9/usage?cedar_ember=0&skip_spend=0": "http://127.0.0.1:9/usage?cedar_ember=0&skip_spend=0",
		"%zz": "%zz",
	}
	for in, want := range cases {
		if got := withGrantQuery(in); got != want {
			t.Errorf("withGrantQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientFetchReadsResetGrants(t *testing.T) {
	client := newTestClient(respondWith(200, fixture(t, "usage_reset_grants.json")), time.Second)
	snap, err := client.Fetch(context.Background(), "auth-1", "0", testToken)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Resets == nil || len(snap.Resets.Grants) != 3 {
		t.Fatalf("Resets = %+v, want three grants", snap.Resets)
	}
	if len(snap.Windows) != 3 {
		t.Errorf("windows = %d, want 3", len(snap.Windows))
	}

	client = newTestClient(respondWith(200, fixture(t, "usage_early_week.json")), time.Second)
	if snap, _ = client.Fetch(context.Background(), "auth-1", "0", testToken); snap.Resets != nil {
		t.Errorf("Resets = %+v from a body with no grants block, want nil", snap.Resets)
	}
}

func timeEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
