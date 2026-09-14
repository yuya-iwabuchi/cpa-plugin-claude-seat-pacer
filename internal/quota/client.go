package quota

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/httpx"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// MaxBodyBytes caps the usage response. Fetch rejects a body past it, so a
// response the Doer hands back whole is never parsed past this size; a Doer
// that reads only MaxBodyBytes+1 bytes also keeps an endless stream from
// exhausting the host, and the one the plugin ships does not.
const MaxBodyBytes = 256 << 10

// userAgent identifies the caller to Anthropic as an OAuth CLI client, which
// is the surface a subscription credential is entitled to use.
const userAgent = "claude-cli/1.0.60 (external, cli)"

// oauthBeta gates the OAuth usage endpoint.
const oauthBeta = "oauth-2025-04-20"

// Throttle retry. The endpoint throttles the caller rather than the
// credential, so a 429 arrives for every seat at once and leaves the pool with
// no reading at all. One short retry recovers from a brief throttle within the
// same poll; anything longer is the next poll's job, which is why a
// Retry-After past retryAfterCap skips the retry rather than waiting it out.
const (
	retryBackoff  = 900 * time.Millisecond
	retryAfterCap = 5 * time.Second
)

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
// ObservedAt is stamped with this client's clock once the response is in
// hand; the poller re-stamps it with the plugin's own clock, which is the one
// staleness is judged against.
func (c *Client) Fetch(ctx context.Context, authID, authIndex, accessToken string) (model.AuthSnapshot, error) {
	snap := model.AuthSnapshot{
		AuthID:    authID,
		AuthIndex: authIndex,
		Source:    model.SourceUsageEndpoint,
	}

	body, err := c.get(ctx, accessToken)
	if wait, ok := retryWait(err); ok {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
			body, err = c.get(ctx, accessToken)
		}
	}
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

// retryWait reports how long to wait before one retry of err, and whether to
// retry at all. Only a throttle is retried: an auth failure, a bad body and a
// timeout all mean the same thing on a second attempt, and a timeout has
// already spent the deadline the retry would need.
func retryWait(err error) (time.Duration, bool) {
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Category != CategoryRateLimited {
		return 0, false
	}
	switch {
	case fe.RetryAfter <= 0:
		return retryBackoff, true
	case fe.RetryAfter > retryAfterCap:
		return 0, false
	default:
		return fe.RetryAfter, true
	}
}

// retryAfterOf reads a Retry-After header as a duration. Only the delta-seconds
// form is read; the HTTP-date form needs a trusted clock difference this has no
// use for, and a value it cannot read reports 0, which leaves the default
// backoff in charge.
func retryAfterOf(h map[string][]string) time.Duration {
	raw, ok := httpx.NewIndex(h).Lookup("Retry-After")
	if !ok {
		return 0
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
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
		var fe *FetchError
		if errors.As(err, &fe) && fe.Category == CategoryRateLimited {
			fe.RetryAfter = retryAfterOf(got.resp.Header)
		}
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
