package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/quota"
)

// HostFunc invokes one host callback and returns the raw response envelope.
// The cgo shim supplies the real one; tests supply a fake.
type HostFunc func(method string, payload []byte) ([]byte, error)

// host is the typed bridge over HostFunc.
//
// A host callback is synchronous and takes no context, so a callback that has
// to respect a deadline runs on its own goroutine and is abandoned when the
// deadline passes. tracker counts the abandoned goroutines, because the host
// frees its callback table and dlcloses this library as soon as
// cliproxy_plugin_shutdown returns: a goroutine that wakes after that runs
// plugin code in unmapped memory.
type host struct {
	call    HostFunc
	tracker *callTracker
}

func newHost(call HostFunc) host {
	return host{call: call, tracker: newCallTracker()}
}

// callTracker counts the host callbacks running on abandoned goroutines and
// publishes a channel that is closed exactly while that count is zero.
//
// A drain waits on the channel it observed rather than on the live count, so it
// needs no goroutine of its own — nothing to leak when its deadline passes —
// and a callback that starts while it waits neither disturbs it nor holds it
// open. Both matter because drain and spawn run concurrently: plugin.quiesce
// arrives on a host goroutine while a management refresh is still polling.
type callTracker struct {
	mu   sync.Mutex
	live int
	idle chan struct{}
}

func newCallTracker() *callTracker {
	tracker := &callTracker{idle: make(chan struct{})}
	close(tracker.idle)
	return tracker
}

func (t *callTracker) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.live == 0 {
		t.idle = make(chan struct{})
	}
	t.live++
}

func (t *callTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live--
	if t.live == 0 {
		close(t.idle)
	}
}

// idleC is closed once the callbacks outstanding when it was read have all
// returned.
func (t *callTracker) idleC() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.idle
}

// invoke marshals payload, calls the host, and decodes the result member into
// out. A nil call function reports every callback as failed, which is how a
// test that must reach no host is wired; the shim always supplies one, and
// cliproxyPluginShutdown drops the C-side host pointer rather than this, so a
// later callback fails inside the shim. It blocks for as long as the host
// takes, so a caller with a deadline uses invokeCtx.
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

// invokeCtx runs one host callback on a tracked goroutine and gives up on it
// when ctx is done, so a wedged host costs one parked goroutine rather than a
// stalled caller. The result member is decoded on the caller's goroutine only
// after the call has returned, so an abandoned call never writes into out.
func (h host) invokeCtx(ctx context.Context, method string, payload any, out any) error {
	if h.call == nil {
		return fmt.Errorf("%s: host callbacks are unavailable", method)
	}
	type outcome struct {
		result json.RawMessage
		err    error
	}
	done := make(chan outcome, 1)
	h.spawn(func() {
		var result json.RawMessage
		err := h.invoke(method, payload, &result)
		done <- outcome{result: result, err: err}
	})
	select {
	case got := <-done:
		if got.err != nil {
			return got.err
		}
		if out == nil || len(got.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(got.result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// spawn runs fn on a tracked goroutine.
func (h host) spawn(fn func()) {
	if h.tracker == nil {
		go fn()
		return
	}
	h.tracker.begin()
	go func() {
		defer h.tracker.end()
		fn()
	}()
}

// drain waits for the callbacks outstanding when it starts and gives up after
// timeout. A host call cannot be cancelled, so a host that never answers must
// not hold the unload open for good.
func (h host) drain(timeout time.Duration) {
	if h.tracker == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-h.tracker.idleC():
	case <-timer.C:
	}
}

// hostLogTimeout bounds one host.log call.
const hostLogTimeout = 2 * time.Second

// log emits a line through the host's logger. Levels outside the host's
// vocabulary fall through to debug, so callers use info, warn, or error.
//
// The line is fire-and-forget, so a host that stops answering costs a dropped
// line rather than a parked caller: the poll loop logs between the steps that
// plugin.shutdown waits on, and the panic guard logs for every method
// including scheduler.pick, which the host gives no timeout.
func (h host) log(level, message string, fields map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), hostLogTimeout)
	defer cancel()
	_ = h.invokeCtx(ctx, MethodHostLog, HostLogRequest{Level: level, Message: message, Fields: fields}, nil)
}

// authList reports every credential the host knows about.
func (h host) authList(ctx context.Context) ([]HostAuthFileEntry, error) {
	var resp HostAuthListResponse
	if err := h.invokeCtx(ctx, MethodHostAuthList, HostAuthListRequest{}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// authGet returns the raw credential file for an auth index. The result
// carries tokens: callers read one field and drop the rest.
func (h host) authGet(ctx context.Context, authIndex string) (HostAuthGetResponse, error) {
	var resp HostAuthGetResponse
	err := h.invokeCtx(ctx, MethodHostAuthGet, HostAuthGetRequest{AuthIndex: authIndex}, &resp)
	return resp, err
}

// hostDoer is a quota.Doer that routes through host.http.do, so a usage fetch
// inherits the host's proxy and TLS configuration.
type hostDoer struct {
	h host
}

func (d hostDoer) Do(ctx context.Context, req quota.Request) (quota.Response, error) {
	headers := make(http.Header, len(req.Header))
	for name, value := range req.Header {
		headers.Set(name, value)
	}
	var resp HostHTTPResponse
	err := d.h.invokeCtx(ctx, MethodHostHTTPDo, HostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: headers,
	}, &resp)
	if err != nil {
		return quota.Response{}, err
	}
	return quota.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Headers,
		Body:       resp.Body,
	}, nil
}
