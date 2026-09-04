package quota

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// MaxBodyBytes caps the usage response. A Doer stops reading here and Fetch
// rejects a longer body, so an endpoint that streams without end cannot
// exhaust the host.
const MaxBodyBytes = 256 << 10

// userAgent identifies the caller to Anthropic as an OAuth CLI client, which
// is the surface a subscription credential is entitled to use.
const userAgent = "claude-cli/1.0.60 (external, cli)"

// oauthBeta gates the OAuth usage endpoint.
const oauthBeta = "oauth-2025-04-20"

// Client reads one credential's quota from the OAuth usage endpoint.
type Client struct {
	doer    Doer
	url     string
	timeout time.Duration
	// now is injectable so tests can pin a snapshot's observation instant.
	now func() time.Time
}

// NewClient returns a client that fetches through d. An empty usageURL falls
// back to model.DefaultUsageURL; a non-positive timeout leaves the caller's
// context deadline in sole charge.
func NewClient(d Doer, usageURL string, timeout time.Duration) *Client {
	if usageURL == "" {
		usageURL = model.DefaultUsageURL
	}
	return &Client{doer: d, url: usageURL, timeout: timeout, now: time.Now}
}

// Fetch reads one credential's quota windows.
//
// The returned snapshot is usable in both outcomes: on failure it carries Err
// and no windows, so a caller can hand it straight to Store.Put, which keeps
// the credential's prior readings and updates only the error.
func (c *Client) Fetch(ctx context.Context, authID, authIndex, accessToken string) (model.AuthSnapshot, error) {
	snap := model.AuthSnapshot{
		AuthID:     authID,
		AuthIndex:  authIndex,
		ObservedAt: c.now(),
		Source:     model.SourceUsageEndpoint,
	}

	windows, err := c.read(ctx, accessToken, snap.ObservedAt)
	if err != nil {
		snap.Err = err.Error()
		return snap, err
	}
	snap.Windows = windows
	return snap, nil
}

func (c *Client) read(ctx context.Context, accessToken string, now time.Time) ([]model.Window, error) {
	if c.doer == nil {
		return nil, &FetchError{Category: CategoryTransport, Detail: "no doer configured"}
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	resp, err := c.doer.Do(ctx, Request{
		Method: "GET",
		URL:    c.url,
		Header: map[string]string{
			"Authorization":  "Bearer " + accessToken,
			"Content-Type":   "application/json",
			"anthropic-beta": oauthBeta,
			"User-Agent":     userAgent,
		},
	})
	if err != nil {
		category := CategoryTransport
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// Shutdown cancellation shares this category: routing treats both
			// as "no reading this round" and the set carries no third name.
			category = CategoryTimeout
		}
		return nil, &FetchError{Category: category, Detail: "request failed", cause: scrub(err, accessToken)}
	}

	if err := statusError(resp.StatusCode); err != nil {
		return nil, err
	}
	if len(resp.Body) > MaxBodyBytes {
		return nil, &FetchError{
			Category: CategoryOversize,
			Status:   resp.StatusCode,
			Detail:   "response exceeds the body cap",
		}
	}
	return ParseUsagePayload(resp.Body, now)
}

// statusError classifies a response status, and reports nil for one that
// carries a body worth parsing.
//
// A 3xx is refused rather than followed: the request carries a bearer token,
// and a redirect target is not the host this credential was issued for. The
// injected Doer may follow redirects on its own, so this only catches the ones
// it surfaces.
func statusError(code int) error {
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == 401:
		return &FetchError{Category: CategoryAuth, Status: code, Detail: "credential rejected"}
	case code == 403:
		return &FetchError{Category: CategoryForbidden, Status: code, Detail: "endpoint not permitted for this credential"}
	case code == 429:
		return &FetchError{Category: CategoryRateLimited, Status: code, Detail: "usage endpoint throttled"}
	case code >= 300 && code < 400:
		return &FetchError{Category: CategoryBadStatus, Status: code, Detail: "redirect refused"}
	default:
		return &FetchError{Category: CategoryBadStatus, Status: code, Detail: "unexpected status"}
	}
}

// redacted stands in for a secret removed from an error message.
const redacted = "[redacted]"

// scrub removes the credential from a transport error, because a Doer is free
// to quote the request it was handed and this error reaches logs. The composed
// Authorization value goes first so a scrubbed message carries no header shape
// either. Flattening the error chain is the price, and it is only paid on the
// path where a secret was actually present.
func scrub(err error, accessToken string) error {
	if err == nil || accessToken == "" {
		return err
	}
	message := err.Error()
	cleaned := message
	for _, secret := range []string{"Bearer " + accessToken, accessToken} {
		cleaned = strings.ReplaceAll(cleaned, secret, redacted)
	}
	if cleaned == message {
		return err
	}
	return errors.New(cleaned)
}
