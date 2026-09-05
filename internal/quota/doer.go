// Package quota turns Anthropic quota observations into model.AuthSnapshot
// values: it reads the OAuth usage endpoint, parses the unified rate-limit
// response headers, and holds the newest snapshot per credential.
//
// The package does no networking of its own: every request goes through a
// Doer, so the runtime supplies the host's HTTP path and tests supply a stub.
package quota

import "context"

// Doer performs one HTTP request. It is this package's only path to the
// network, so a Doer that reaches nothing keeps the package offline.
type Doer interface {
	Do(ctx context.Context, req Request) (Response, error)
}

// Request is a Doer's input. Header carries one value per name, which is all
// the usage endpoint needs.
type Request struct {
	Method string
	URL    string
	Header map[string]string
}

// Response is a Doer's output. Body is already read.
type Response struct {
	StatusCode int
	Header     map[string][]string
	Body       []byte
}
