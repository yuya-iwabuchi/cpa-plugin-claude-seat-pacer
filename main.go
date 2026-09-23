// Command claude-seat-pacer is a CLIProxyAPI plugin that uses up each Claude
// OAuth subscription seat's weekly quota before it resets: every new
// conversation goes to the seat furthest behind a plan that spends its quota
// steadily and finishes a little early, and each conversation stays on one seat
// so Anthropic prompt caches keep hitting.
//
// This file is the C ABI boundary only: it installs the plugin vtable, guards
// every entry point against panics, and forwards each call to
// internal/runtime, which holds all behaviour.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

// stored_host is read once into a local and used only through that local.
// cliproxy_plugin_shutdown nulls it after Shutdown, whose drain gives up after
// a bounded wait, so a callback can still arrive here; a single load is the
// narrowest window a plain pointer allows, since C89 offers no atomic.
static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	const cliproxy_host_api* host = stored_host;
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	const cliproxy_host_api* host = stored_host;
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"math"
	"unsafe"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/runtime"
	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/web"
)

// The status route serves a forced usage read only to a Source that offers
// one, and does so through a type assertion that would otherwise fail in
// silence, leaving the page's sync button inert.
var _ web.Syncer = (*runtime.Plugin)(nil)

// Plugin identity. The host rejects a plugin with any of these blank. The
// Makefile reads pluginVersion from this line to name the installed library.
const (
	pluginName    = "claude-seat-pacer"
	pluginVersion = "0.1.0"
	pluginAuthor  = "yuya-iwabuchi"
	pluginRepo    = "https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer"
)

// configFields are the knobs the Management Center renders inputs for. The
// full set lives in model.Config; these are the ones an operator tunes.
// enabled is absent: the host renders its own toggle for every plugin, and
// loads no disabled plugin at all.
var configFields = []runtime.ConfigField{
	{Name: "providers", Type: "array", Description: "Provider keys the plugin governs. Default [claude]."},
	{Name: "affinity.ttl", Type: "string", Description: "Idle time before a conversation's credential binding expires. Default 1h."},
	{Name: "pace.shape", Type: "enum", EnumValues: []string{model.ShapeLinear, model.ShapePower, model.ShapeSigmoid}, Description: "Target curve shape. Linear spends evenly, power bends the curve by pace.curve-exponent, and sigmoid holds back early and eases off near the end. Default linear."},
	{Name: "pace.curve-exponent", Type: "number", Description: "Exponent for the power shape; above 1.0 holds back early. Selects the power shape when pace.shape is unset, and is ignored by the sigmoid shape. Default 1.0."},
	{Name: "pace.steepness", Type: "number", Description: "Slope through the sigmoid's midpoint. Ignored by the other shapes. Default 8.0."},
	{Name: "pace.landing-target", Type: "number", Description: "How far each seat's plan runs ahead of an even pace. 1.10 finishes the week's quota about 9% early on the linear shape, which leans toward a seat near its reset; 1.0 finishes at the reset. Default 1.10."},
	{Name: "quota.poll-interval", Type: "string", Description: "How often each credential's usage endpoint is read. Default 2m; a value below 30s falls back to the default."},
	{Name: "web.enabled", Type: "boolean", Description: "Serve the status page, and the management route its data comes from. Default true."},
}

var plugin = newPlugin()

// newPlugin builds the runtime and installs the status app, which serves the
// page on the plugin's resource route and its data on the page-status
// management route; the runtime declares both only while web.enabled is on.
func newPlugin() *runtime.Plugin {
	p := runtime.New(runtime.Options{
		Name:         pluginName,
		Version:      pluginVersion,
		Author:       pluginAuthor,
		Repository:   pluginRepo,
		ConfigFields: configFields,
		Host:         callHost,
	})
	p.SetResourceHandler(web.NewHandler(p))
	return p
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) (rc C.int) {
	defer func() {
		if recover() != nil {
			rc = 1
		}
	}()
	if api == nil {
		return 1
	}
	C.store_host_api(host)
	api.abi_version = C.uint32_t(runtime.ABIVersion)
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	// The host's recover runs in its own runtime and cannot catch a panic
	// raised inside this library; an escaped panic terminates the proxy. The
	// runtime guards its own dispatch too, so this covers the shim itself.
	defer func() {
		if r := recover(); r != nil {
			writeResponse(response, errorEnvelope("plugin_panic", fmt.Sprintf("recovered: %v", r)))
			rc = 1
		}
	}()
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	if bufferTooLarge(uint64(requestLen)) {
		writeResponse(response, errorEnvelope("invalid_request", "request envelope is too large"))
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, ok := plugin.Call(C.GoString(method), payload)
	writeResponse(response, raw)
	if !ok {
		return 1
	}
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	defer func() { _ = recover() }()
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	// The runtime is joined first, so the callbacks it still has outstanding
	// finish while the host's callback table is valid. The pointer is dropped
	// after: a callback that outlived the join then fails instead of touching
	// the struct the host frees as soon as this returns.
	plugin.Shutdown()
	C.store_host_api(nil)
}

// errorEnvelope is the shim's own failure encoding, for the paths where the
// runtime was never reached. The encoder builds it because Go quoting is not
// JSON quoting: a message carrying a non-printable or invalid-UTF-8 byte still
// has to survive the host's decode, and a decode failure there fails the
// request outright instead of declining it.
func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(map[string]any{
		"ok":    false,
		"error": map[string]string{"code": code, "message": message},
	})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"internal","message":"failed to encode error"}}`)
	}
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHost invokes a host callback and returns the raw response envelope.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var request *C.uint8_t
	if len(payload) > 0 {
		request = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(request))
	}

	var response C.cliproxy_buffer
	if rc := C.call_host_api(cMethod, request, C.size_t(len(payload)), &response); rc != 0 {
		if response.ptr != nil {
			C.free_host_buffer(response.ptr, response.len)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if response.ptr == nil {
		return nil, nil
	}
	defer C.free_host_buffer(response.ptr, response.len)
	if bufferTooLarge(uint64(response.len)) {
		return nil, fmt.Errorf("host callback %s returned an oversized response", method)
	}
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}

// bufferTooLarge reports whether a C buffer length is past what C.GoBytes
// accepts. Its length argument is a C int, so a size_t beyond MaxInt32 wraps
// negative there and runtime.gobytes throws — a fatal error, not a panic, so
// no recover in this file reaches it.
func bufferTooLarge(n uint64) bool {
	return n > math.MaxInt32
}
