package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// The identifiers a reset claim carries, in the forms the endpoint accepts.
// A request id the endpoint has already answered gets that answer again, so a
// claim retried with its first request id uses at most one reset.
var (
	grantIDPattern   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	orgIDPattern     = regexp.MustCompile(`^[0-9A-Fa-f-]{36}$`)
)

// claimTimeout is the least time one claim gets, which the endpoint answers
// only once the windows are cleared; a longer read timeout applies instead.
const claimTimeout = 25 * time.Second

// resetProgram names the grant program a claim draws on: the one the usage
// read's grants block reports.
const resetProgram = "cedar_ember"

type claimPayload struct {
	Result        string   `json:"result"`
	Reason        string   `json:"reason"`
	ResetsLeft    *int     `json:"resets_left"`
	Cleared       []string `json:"cleared"`
	CooldownUntil string   `json:"cooldown_until"`
}

// ErrClaimInput reports a claim whose organization, grant or request id is
// not in the form the endpoint accepts. Nothing is sent.
var ErrClaimInput = fmt.Errorf("quota: claim names a malformed organization, grant or request id: %w", model.ErrClaimNotSent)

// ClaimReset uses one of a grant's resets on the account accessToken belongs
// to, through the organization endpoint on the usage URL's host. It sends
// nothing for a malformed id. A result that is not one of model's Claim
// values reads as model.ClaimUnavailable. A response that is not 2xx is a
// *FetchError, and so is a 2xx answer it cannot read, of CategoryBadJSON.
func (c *Client) ClaimReset(ctx context.Context, accessToken, orgID, grantID, requestID string) (model.ResetClaim, error) {
	if !orgIDPattern.MatchString(orgID) || !grantIDPattern.MatchString(grantID) || !requestIDPattern.MatchString(requestID) {
		return model.ResetClaim{}, ErrClaimInput
	}
	u, err := url.Parse(c.url)
	if err != nil {
		return model.ResetClaim{}, &FetchError{Category: CategoryTransport, Detail: "usage URL unreadable"}
	}
	u.Path = "/api/organizations/" + orgID + "/reset_rate_limits"
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	body, _ := json.Marshal(map[string]string{"program": resetProgram, "grant_id": grantID, "request_id": requestID})

	claim := *c
	claim.timeout = max(claimTimeout, c.timeout)
	raw, err := claim.send(ctx, Request{Method: "POST", URL: u.String(), Header: c.header(accessToken), Body: body}, accessToken)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) && fe.Category == CategoryRateLimited {
			fe.Detail = "claim throttled"
		}
		return model.ResetClaim{}, err
	}
	var p claimPayload
	if json.Unmarshal(raw, &p) != nil || p.Result == "" {
		return model.ResetClaim{}, &FetchError{Category: CategoryBadJSON, Detail: "claim answer unreadable"}
	}
	out := model.ResetClaim{
		Result:        normalizeToken(p.Result),
		Reason:        normalizeToken(p.Reason),
		ResetsLeft:    p.ResetsLeft,
		Cleared:       []model.WindowRef{},
		CooldownUntil: optionalTime(p.CooldownUntil),
	}
	switch out.Result {
	case model.ClaimReset, model.ClaimAlreadyUsed, model.ClaimNotLimited, model.ClaimCooldown, model.ClaimIneligible:
	default:
		out.Result = model.ClaimUnavailable
	}
	for _, name := range p.Cleared {
		if key, ok := claimWindow(normalizeToken(name)); ok {
			ref := model.WindowRef{Kind: key.kind, Scope: key.scope}
			if !slices.Contains(out.Cleared, ref) {
				out.Cleared = append(out.Cleared, ref)
			}
		}
	}
	return out, nil
}
