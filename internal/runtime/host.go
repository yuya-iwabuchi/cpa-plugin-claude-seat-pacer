package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/quota"
)

// HostFunc invokes one host callback and returns the raw response envelope.
// The cgo shim supplies the real one; tests supply a fake.
type HostFunc func(method string, payload []byte) ([]byte, error)

// host is the typed bridge over HostFunc.
type host struct {
	call HostFunc
}

// invoke marshals payload, calls the host, and decodes the result member into
// out. A nil call function reports every callback as failed, which is the
// state after cliproxyPluginShutdown has run.
func (h host) invoke(method string, payload any, out any) error {
	if h.call == nil {
		return fmt.Errorf("%s: host callbacks are unavailable", method)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	raw, err := h.call(method, encoded)
	if err != nil {
		return err
	}
	if err := unwrapEnvelope(raw, out); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

// log emits a line through the host's logger. Levels outside the host's
// vocabulary fall through to debug, so callers use info, warn, or error. A
// failure is ignored: logging must never fail a hook.
func (h host) log(level, message string, fields map[string]any) {
	_ = h.invoke(MethodHostLog, HostLogRequest{Level: level, Message: message, Fields: fields}, nil)
}

// authList reports every credential the host knows about.
func (h host) authList() ([]HostAuthFileEntry, error) {
	var resp HostAuthListResponse
	if err := h.invoke(MethodHostAuthList, HostAuthListRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// authGet returns the raw credential file for an auth index. The result
// carries tokens: callers read one field and drop the rest.
func (h host) authGet(authIndex string) (HostAuthGetResponse, error) {
	var resp HostAuthGetResponse
	err := h.invoke(MethodHostAuthGet, HostAuthGetRequest{AuthIndex: authIndex}, &resp)
	return resp, err
}

// hostDoer is a quota.Doer that routes through host.http.do, so a usage fetch
// inherits the host's proxy and TLS configuration.
//
// The host callback is synchronous and has no context parameter. Do runs it
// on its own goroutine and returns as soon as ctx is done, so a hung host call
// costs one parked goroutine rather than a stalled poll loop; the goroutine
// exits when the host eventually answers.
type hostDoer struct {
	h host
}

func (d hostDoer) Do(ctx context.Context, req quota.Request) (quota.Response, error) {
	type outcome struct {
		resp quota.Response
		err  error
	}
	headers := make(http.Header, len(req.Header))
	for name, value := range req.Header {
		headers.Set(name, value)
	}
	done := make(chan outcome, 1)
	go func() {
		var resp HostHTTPResponse
		err := d.h.invoke(MethodHostHTTPDo, HostHTTPRequest{
			Method:  req.Method,
			URL:     req.URL,
			Headers: headers,
		}, &resp)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		done <- outcome{resp: quota.Response{
			StatusCode: resp.StatusCode,
			Header:     resp.Headers,
			Body:       resp.Body,
		}}
	}()
	select {
	case out := <-done:
		return out.resp, out.err
	case <-ctx.Done():
		return quota.Response{}, ctx.Err()
	}
}
