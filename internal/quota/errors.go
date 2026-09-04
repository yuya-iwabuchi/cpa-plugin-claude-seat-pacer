package quota

import (
	"errors"
	"fmt"
	"strings"
)

// ErrCategory classifies a fetch failure. The status UI renders the category
// verbatim, so the set stays small and stable.
type ErrCategory string

const (
	// CategoryAuth is a credential the endpoint refuses (401).
	CategoryAuth ErrCategory = "auth"
	// CategoryForbidden is a credential without access to the endpoint (403).
	CategoryForbidden ErrCategory = "forbidden"
	// CategoryRateLimited is the endpoint throttling the poller (429).
	CategoryRateLimited ErrCategory = "rate-limited"
	// CategoryTransport is a failure below HTTP: DNS, dial, TLS, proxy.
	CategoryTransport ErrCategory = "transport"
	// CategoryTimeout is a deadline or cancellation on the fetch context.
	CategoryTimeout ErrCategory = "timeout"
	// CategoryBadStatus is any other unusable status, a refused redirect among
	// them.
	CategoryBadStatus ErrCategory = "bad-status"
	// CategoryBadJSON is a body that does not parse, or parses into no
	// readable window.
	CategoryBadJSON ErrCategory = "bad-json"
	// CategoryOversize is a body past MaxBodyBytes.
	CategoryOversize ErrCategory = "oversize"
)

// FetchError reports a usage fetch failure and its category.
//
// Its message is built only from values this package authors: a category, an
// HTTP status, and a fixed detail phrase. Response content and header values,
// the access token among them, never reach it, because the message lands in
// logs and in the status UI.
type FetchError struct {
	Category ErrCategory
	// Status is the HTTP status when the failure came from a response, and 0
	// otherwise.
	Status int
	// Detail is a fixed phrase naming the failing step.
	Detail string
	cause  error
}

func (e *FetchError) Error() string {
	var b strings.Builder
	b.WriteString("quota: ")
	b.WriteString(string(e.Category))
	if e.Status != 0 {
		fmt.Fprintf(&b, " (http %d)", e.Status)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.cause != nil {
		b.WriteString(": ")
		b.WriteString(e.cause.Error())
	}
	return b.String()
}

// Unwrap exposes a transport cause. Response-derived failures carry none: a
// json error quotes the bytes it choked on, so it is classified and dropped
// rather than wrapped.
func (e *FetchError) Unwrap() error { return e.cause }

// Category reports err's failure category, or the empty string when err is not
// a FetchError.
func Category(err error) ErrCategory {
	var fe *FetchError
	if errors.As(err, &fe) {
		return fe.Category
	}
	return ""
}
