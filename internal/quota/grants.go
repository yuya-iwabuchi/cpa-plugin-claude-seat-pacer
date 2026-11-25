package quota

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// grantQuery asks the usage endpoint for the account's limit-reset grants,
// which it reports under cedar_ember, and to leave out the spend block, which
// nothing here reads. The windows come back unchanged.
var grantQuery = [][2]string{{"cedar_ember", "1"}, {"skip_spend", "1"}}

// withGrantQuery appends to a usage URL each grantQuery pair its query does
// not already set, leaving the rest of the URL as written, and returns a URL
// it cannot parse unchanged.
func withGrantQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	add := url.Values{}
	for _, kv := range grantQuery {
		if !q.Has(kv[0]) {
			add.Add(kv[0], kv[1])
		}
	}
	if len(add) == 0 {
		return raw
	}
	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += add.Encode()
	return u.String()
}

// grantsPayload is the cedar_ember block of a usage body, each grant kept
// raw so one it cannot read leaves the others.
type grantsPayload struct {
	CedarEmber *struct {
		Grants []json.RawMessage `json:"grants"`
	} `json:"cedar_ember"`
}

type grantPayload struct {
	ID                string   `json:"id"`
	Type              string   `json:"type"`
	ResetsLeft        *int     `json:"resets_left"`
	EndsAt            string   `json:"ends_at"`
	Clears            []string `json:"clears"`
	Paused            bool     `json:"paused"`
	UsableNow         bool     `json:"usable_now"`
	GivesBackWeekly   bool     `json:"gives_back_weekly"`
	UnavailableReason string   `json:"unavailable_reason"`
}

// ParseResetGrants reads the limit-reset grants from a usage body, and
// returns nil for a body that carries no grants block or one whose grants
// list it cannot read. A grant it cannot read, or that names no id or count,
// is left out. The grants never decide whether a read succeeded: a malformed
// block leaves the windows the same body carries in charge.
func ParseResetGrants(b []byte) *model.ResetGrants {
	var p grantsPayload
	if err := json.Unmarshal(b, &p); err != nil || p.CedarEmber == nil {
		return nil
	}
	out := &model.ResetGrants{Grants: []model.ResetGrant{}}
	for _, raw := range p.CedarEmber.Grants {
		var g grantPayload
		if err := json.Unmarshal(raw, &g); err != nil || g.ID == "" || g.ResetsLeft == nil {
			continue
		}
		grant := model.ResetGrant{
			ID:          g.ID,
			Type:        normalizeToken(g.Type),
			Left:        max(*g.ResetsLeft, 0),
			EndsAt:      optionalTime(g.EndsAt),
			Clears:      []model.WindowRef{},
			UsableNow:   g.UsableNow,
			Paused:      g.Paused,
			GivesBack:   g.GivesBackWeekly,
			Unavailable: normalizeToken(g.UnavailableReason),
		}
		for _, name := range g.Clears {
			key, ok := claimWindow(normalizeToken(name))
			if !ok {
				continue
			}
			ref := model.WindowRef{Kind: key.kind, Scope: key.scope}
			if !slices.Contains(grant.Clears, ref) {
				grant.Clears = append(grant.Clears, ref)
			}
		}
		grant.Family = grantFamily(grant.Clears)
		out.Grants = append(out.Grants, grant)
	}
	return out
}

// grantFamily names the one family whose weekly cap a grant clears, when it
// clears that cap and not the 7-day window.
func grantFamily(clears []model.WindowRef) string {
	family := ""
	for _, ref := range clears {
		switch {
		case ref.Kind == model.WindowWeekly:
			return ""
		case ref.Kind == model.WindowWeeklyScoped && family == "":
			family = ref.Scope
		case ref.Kind == model.WindowWeeklyScoped:
			return ""
		}
	}
	return family
}

// optionalTime parses an endpoint timestamp, and reports nil for an empty or
// unreadable one.
func optionalTime(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil
	}
	return &t
}
