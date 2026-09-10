// Command cpa-claude-quota-scheduler is a CLIProxyAPI plugin that routes
// requests across multiple Claude OAuth credentials by burn pace while keeping
// each conversation pinned to one credential so Anthropic prompt caches keep
// hitting.
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

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/runtime"
	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/web"
)

// The status route serves a forced usage read only to a Source that offers
// one, and does so through a type assertion that would otherwise fail in
// silence, leaving the page's sync button inert.
var _ web.Syncer = (*runtime.Plugin)(nil)

// Plugin identity. The host rejects a plugin with any of these blank. The
// Makefile reads pluginVersion from this line to name the installed library.
const (
	pluginName    = "cpa-claude-quota-scheduler"
	pluginVersion = "0.1.0"
	pluginAuthor  = "yuya-iwabuchi"
	pluginRepo    = "https://github.com/yuya-iwabuchi/cpa-claude-quota-scheduler"
)

// configFields are the knobs the Management Center renders inputs for. The
// full set lives in model.Config; these are the ones an operator tunes.
var configFields = []runtime.ConfigField{
	{Name: "enabled", Type: "boolean", Description: "Route governed requests. Off leaves the host's own selector in charge."},
	{Name: "providers", Type: "array", Description: "Provider keys the plugin governs. Default [claude]."},
	{Name: "affinity.ttl", Type: "string", Description: "Idle time before a conversation's credential binding expires. Default 1h."},
	{Name: "pace.shape", Type: "string", Description: "Target curve shape: linear, power or sigmoid. Default linear."},
	{Name: "pace.curve-exponent", Type: "number", Description: "Exponent for the power shape; above 1.0 holds back early. Selects the power shape when pace.shape is unset, and is ignored by the sigmoid shape. Default 1.0."},
	{Name: "pace.steepness", Type: "number", Description: "Slope through the sigmoid's midpoint. Ignored by the other shapes. Default 8.0."},
	{Name: "pace.landing-target", Type: "number", Description: "Utilization the curve aims for at window end, clamped to full. Above 1.0 favours a credential near its reset, whose budget expires soonest. Default 1.10."},
	{Name: "quota.poll-interval", Type: "string", Description: "How often each credential's usage endpoint is read. Default 2m, minimum 30s."},
	{Name: "web.enabled", Type: "boolean", Description: "Serve the status page on the plugin's resource routes. Default true."},
}

var plugin = newPlugin()

// newPlugin builds the runtime and installs the status app on its resource
// routes. The app is what those routes serve; the runtime declares them only
// while web.enabled is on.
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
	return C.GoBytes(response.ptr, C.int(response.len)), nil
}
