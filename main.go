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
	"fmt"
	"unsafe"

	"github.com/yuya-iwabuchi/cpa-claude-quota-scheduler/internal/runtime"
)

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
	{Name: "pace.curve-exponent", Type: "number", Description: "Target curve exponent; 1.0 spends evenly, above 1.0 holds back early. Default 1.0."},
	{Name: "pace.landing-target", Type: "number", Description: "Utilization the curve aims for at window end. Default 1.0."},
	{Name: "pace.hard-cutoff", Type: "number", Description: "Utilization at or above which a credential is ineligible for a cold pick. Default 0.98."},
	{Name: "quota.poll-interval", Type: "string", Description: "How often each credential's usage endpoint is read. Default 2m, minimum 30s."},
	{Name: "web.enabled", Type: "boolean", Description: "Serve the status page on the plugin's resource routes. Default true."},
}

var plugin = runtime.New(runtime.Options{
	Name:         pluginName,
	Version:      pluginVersion,
	Author:       pluginAuthor,
	Repository:   pluginRepo,
	ConfigFields: configFields,
	Host:         callHost,
})

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
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
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	// The host frees the host_api struct after shutdown, so the pointer is
	// dropped first; a callback after this point fails instead of touching
	// freed memory. The poller is joined so no goroutine outlives dlclose.
	C.store_host_api(nil)
	plugin.Shutdown()
}

// errorEnvelope is the shim's own failure encoding, for the paths where the
// runtime was never reached.
func errorEnvelope(code, message string) []byte {
	return []byte(fmt.Sprintf(`{"ok":false,"error":{"code":%q,"message":%q}}`, code, message))
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
