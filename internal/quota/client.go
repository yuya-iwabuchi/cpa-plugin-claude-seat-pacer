package quota

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/model"
)

// MaxBodyBytes caps the usage response. A Doer reads at most MaxBodyBytes+1
// bytes, so Fetch can tell a capped body from a full one and rejects the capped
// one; an endpoint that streams without end therefore cannot exhaust the host.
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
// and ErrCategory and no windows, so a caller can hand it straight to
// Store.Put, which keeps the credential's prior readings and updates only the
// error.
//
// ObservedAt dates the reading rather than the attempt: it is stamped once the
// response is in hand, so a header merge that lands mid round-trip stays the
// newer observation of the windows it covers.
func (c *Client) Fetch(ctx context.Context, authID, authIndex, accessToken string) (model.AuthSnapshot, error) {
	snap := model.AuthSnapshot{
		AuthID:    authID,
		AuthIndex: authIndex,
		Source:    model.SourceUsageEndpoint,
	}

	body, err := c.get(ctx, accessToken)
	snap.ObservedAt = c.now()
	if err == nil {
		snap.Windows, err = ParseUsagePayload(body, snap.ObservedAt)
	}
	if err != nil {
		snap.Windows = nil
		snap.Err = err.Error()
		snap.ErrCategory = string(Category(err))
		return snap, err
	}
	return snap, nil
}

// get performs the usage request and reports the response body.
//
// The Doer runs on its own goroutine and the deadline is enforced here, so a
// Doer that ignores ctx cannot hold the poll loop open: at the deadline get
// reports a timeout and the abandoned goroutine's result is dropped.
func (c *Client) get(ctx context.Context, accessToken string) ([]byte, error) {
	if c.doer == nil {
		return nil, &FetchError{Category: CategoryTransport, Detail: "no doer configured"}
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	type outcome struct {
		resp Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
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
		done <- outcome{resp: resp, err: err}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-ctx.Done():
		return nil, &FetchError{Category: CategoryTimeout, Detail: "request failed", cause: ctx.Err()}
	}

	if got.err != nil {
		category := CategoryTransport
		if errors.Is(got.err, context.DeadlineExceeded) || errors.Is(got.err, context.Canceled) {
			category = CategoryTimeout
		}
		return nil, &FetchError{Category: category, Detail: "request failed", cause: scrub(got.err, accessToken)}
	}
	if err := statusError(got.resp.StatusCode); err != nil {
		return nil, err
	}
	if len(got.resp.Body) > MaxBodyBytes {
		return nil, &FetchError{
			Category: CategoryOversize,
			Status:   got.resp.StatusCode,
			Detail:   "response exceeds the body cap",
		}
	}
	return got.resp.Body, nil
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
