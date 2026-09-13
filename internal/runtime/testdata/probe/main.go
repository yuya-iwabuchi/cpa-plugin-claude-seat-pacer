// Command probe is a CLIProxyAPI plugin that records what the host actually
// sends on each hook, so the wire contract in internal/runtime/wire.go can be
// checked against a running host rather than against the host's source alone.
//
// It declares request_interceptor, scheduler, usage_plugin and management_api,
// injects a marker header in request.intercept_before, and reports at
// scheduler.pick whether that marker survived into SchedulerOptions.Headers.
// Every pick answers Handled:false, so the probe can observe routing but never
// change it.
//
// It decodes every payload, and answers every hook the host contract shapes,
// through the wire types in internal/runtime, so a live host exercises those
// tags rather than a second hand-written copy of them.
//
// Observations are appended as JSON lines to the file named by the stateFile
// build variable and mirrored through host.log.
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
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/runtime"
)

// stateFile is the observation log path, set with -ldflags -X main.stateFile=.
var stateFile string

// captureDir receives the first raw payload seen for each method, one
// <method>.json per file, set with -ldflags -X main.captureDir=. Authorization
// values are redacted before writing.
var captureDir string

// MarkerHeader is the private header the interceptor injects and the pick
// looks for. It is already in http.Header canonical form, so the host's
// merge does not rewrite the key.
const MarkerHeader = "X-Cpa-Probe-Marker"

// logLevel is the level every mirrored observation goes out at. A host running
// with debug:false drops anything below error, so a quieter level would make
// the server log a blank record of the same round trip.
const logLevel = "error"

const (
	pluginName    = "cpa-probe"
	pluginVersion = "0.0.1"
	pluginAuthor  = "yuya-iwabuchi"
	pluginRepo    = "https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer"
)

var (
	stateMu  sync.Mutex
	markerNo atomic.Int64
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(runtime.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	// The host's recover runs in its own runtime and cannot catch a panic
	// raised inside this dynamically loaded library, which would otherwise
	// terminate the whole proxy instead of failing one call.
	defer func() {
		if r := recover(); r != nil {
			record(map[string]any{"hook": "panic", "value": fmt.Sprintf("%v", r)})
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
	name := C.GoString(method)
	capture(name, payload)
	raw, err := handleMethod(name, payload)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
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
	// The host frees the host_api struct after shutdown, so leaving the
	// pointer in place would make any later callback a use-after-free.
	C.store_host_api(nil)
}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case runtime.MethodPluginRegister, runtime.MethodPluginReconfigure:
		return handleLifecycle(method, payload)
	case runtime.MethodRequestInterceptBefore:
		return handleInterceptBefore(payload)
	case runtime.MethodRequestInterceptAfter:
		return handleInterceptAfter(payload)
	case runtime.MethodSchedulerPick:
		return handlePick(payload)
	case runtime.MethodUsageHandle:
		return handleUsage(payload)
	case runtime.MethodManagementRegister:
		return handleManagementRegister()
	case runtime.MethodManagementHandle:
		return handleManagementCall(payload)
	case runtime.MethodPluginShutdown, runtime.MethodPluginQuiesce:
		return okEnvelope(struct{}{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func handleLifecycle(method string, payload []byte) ([]byte, error) {
	var req runtime.LifecycleRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	record(map[string]any{
		"hook":                method,
		"host_schema_version": req.SchemaVersion,
		"config_yaml":         string(req.ConfigYAML),
		"raw_len":             len(payload),
	})
	return okEnvelope(runtime.RegisterResult{
		SchemaVersion: runtime.SchemaVersion,
		Metadata: runtime.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields:     []runtime.ConfigField{},
		},
		Capabilities: map[string]bool{
			runtime.CapabilityRequestInterceptor: true,
			runtime.CapabilityScheduler:          true,
			runtime.CapabilityUsagePlugin:        true,
			runtime.CapabilityManagementAPI:      true,
		},
	})
}

// ManagementPath is the probe's diagnostic route under /v0/management/.
const ManagementPath = "/probe/report"

func handleManagementRegister() ([]byte, error) {
	record(map[string]any{"hook": runtime.MethodManagementRegister})
	return okEnvelope(runtime.ManagementRegistrationResponse{
		Routes: []runtime.ManagementRoute{{
			Method:      http.MethodGet,
			Path:        ManagementPath,
			Description: "Probe observations and host-callback results.",
		}},
	})
}

// handleManagementCall exercises the host auth callbacks off the pick path and
// reports what came back. Only identifiers and JSON key names are recorded;
// credential values never leave the host.
func handleManagementCall(payload []byte) ([]byte, error) {
	var req runtime.ManagementRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	entry := map[string]any{
		"hook":              runtime.MethodManagementHandle,
		"method":            req.Method,
		"path":              req.Path,
		"header_keys":       sortedKeys(req.Headers),
		"query_keys":        sortedKeys(req.Query),
		"callback_id_empty": req.HostCallbackID == "",
	}

	listRaw, err := callHostResult(runtime.MethodHostAuthList, runtime.HostAuthListRequest{HostCallbackID: req.HostCallbackID})
	if err != nil {
		entry["auth_list_error"] = err.Error()
	} else {
		var list runtime.HostAuthListResponse
		if err = json.Unmarshal(listRaw, &list); err != nil {
			entry["auth_list_error"] = "decode: " + err.Error()
		} else {
			entry["auth_list"] = identifiersOf(list.Files)
			if len(list.Files) > 0 {
				entry["auth_get"] = probeAuthGet(list.Files[0].AuthIndex)
			}
		}
	}

	if target := firstValue(req.Query, "http_probe"); target != "" {
		entry["http_do"] = probeHostHTTP(req.HostCallbackID, target)
	}

	record(entry)
	body, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return okEnvelope(runtime.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
	})
}

// identifiersOf projects the auth listing down to the fields that name a
// credential, so an email or a status message never reaches the report.
func identifiersOf(files []runtime.HostAuthFileEntry) []map[string]string {
	out := make([]map[string]string, 0, len(files))
	for _, file := range files {
		out = append(out, map[string]string{
			"id":         file.ID,
			"auth_index": file.AuthIndex,
			"name":       file.Name,
			"type":       file.Type,
			"provider":   file.Provider,
			"status":     file.Status,
		})
	}
	return out
}

// probeHostHTTP routes one GET through the host so the host.http.do request
// and response shapes are exercised against a real host.
func probeHostHTTP(callbackID, target string) map[string]any {
	raw, err := callHostResult(runtime.MethodHostHTTPDo, runtime.HostHTTPRequest{
		HostCallbackID: callbackID,
		Method:         http.MethodGet,
		URL:            target,
		Headers:        http.Header{"X-Cpa-Probe": {"host-http-do"}},
	})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var resp runtime.HostHTTPResponse
	if err = json.Unmarshal(raw, &resp); err != nil {
		return map[string]any{"error": "decode: " + err.Error()}
	}
	return map[string]any{
		"status_code": resp.StatusCode,
		"header_keys": sortedKeys(resp.Headers),
		"body_len":    len(resp.Body),
	}
}

// probeAuthGet reports the shape of a credential payload without its values.
func probeAuthGet(authIndex string) map[string]any {
	raw, err := callHostResult(runtime.MethodHostAuthGet, runtime.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var resp runtime.HostAuthGetResponse
	if err = json.Unmarshal(raw, &resp); err != nil {
		return map[string]any{"error": "decode: " + err.Error()}
	}
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(resp.JSON, &fields)
	return map[string]any{
		"auth_index":  resp.AuthIndex,
		"name":        resp.Name,
		"path_set":    resp.Path != "",
		"json_fields": sortedKeys(fields),
	}
}

// callHostResult unwraps the host's {ok,result,error} envelope.
func callHostResult(method string, payload any) (json.RawMessage, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, err := callHost(method, encoded)
	if err != nil {
		return nil, err
	}
	var env runtime.Envelope
	if err = json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode %s envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s failed", method)
	}
	return env.Result, nil
}

func handleInterceptBefore(payload []byte) ([]byte, error) {
	var req runtime.RequestInterceptRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode intercept_before request: %w", err)
	}
	marker := "probe-" + strconv.FormatInt(markerNo.Add(1), 10) + "-" + req.RequestID
	entry := map[string]any{
		"hook":         runtime.MethodRequestInterceptBefore,
		"request_id":   req.RequestID,
		"model":        req.Model,
		"header_keys":  sortedKeys(req.Headers),
		"body_len":     len(req.Body),
		"body_decoded": len(req.Body) > 0 && req.Body[0] == '{',
		"marker":       marker,
	}
	record(entry)
	hostLog("probe intercept_before", entry)
	return okEnvelope(runtime.RequestInterceptResponse{
		Headers: http.Header{MarkerHeader: {marker}},
	})
}

func handleInterceptAfter(payload []byte) ([]byte, error) {
	var req runtime.RequestInterceptRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode intercept_after request: %w", err)
	}
	record(map[string]any{
		"hook":        runtime.MethodRequestInterceptAfter,
		"request_id":  req.RequestID,
		"to_format":   req.ToFormat,
		"header_keys": sortedKeys(req.Headers),
		"marker":      firstValue(req.Headers, MarkerHeader),
	})
	return okEnvelope(runtime.RequestInterceptResponse{})
}

func handlePick(payload []byte) ([]byte, error) {
	var req runtime.SchedulerPickRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode scheduler pick request: %w", err)
	}
	candidateIDs := make([]string, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		candidateIDs = append(candidateIDs, candidate.ID)
	}
	marker := firstValue(req.Options.Headers, MarkerHeader)
	entry := map[string]any{
		"hook":            runtime.MethodSchedulerPick,
		"provider":        req.Provider,
		"providers":       req.Providers,
		"model":           req.Model,
		"stream":          req.Stream,
		"marker":          marker,
		"marker_present":  marker != "",
		"header_keys":     sortedKeys(req.Options.Headers),
		"metadata_keys":   sortedKeys(req.Options.Metadata),
		"candidate_ids":   candidateIDs,
		"candidate_count": len(req.Candidates),
	}
	if len(req.Candidates) > 0 {
		entry["candidate_priority"] = req.Candidates[0].Priority
		entry["candidate_status"] = req.Candidates[0].Status
		entry["candidate_attribute_keys"] = sortedKeys(req.Candidates[0].Attributes)
		entry["candidate_metadata_nil"] = req.Candidates[0].Metadata == nil
	}
	record(entry)
	// Blocking on a host callback inside the pick is an anti-pattern the real
	// plugin must not copy: scheduler.pick has no timeout, so a stalled call
	// here parks a goroutine for the process lifetime and blocks dlclose. The
	// probe accepts that to get a second, host-side record of the round trip.
	hostLog("probe scheduler_pick", entry)
	// Handled:false is the only answer that cannot alter routing.
	return okEnvelope(runtime.SchedulerPickResponse{Handled: false})
}

func handleUsage(payload []byte) ([]byte, error) {
	var req runtime.UsageRecord
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	record(map[string]any{
		"hook":                  runtime.MethodUsageHandle,
		"auth_id":               req.AuthID,
		"model":                 req.Model,
		"failed":                req.Failed,
		"failure_status":        req.Failure.StatusCode,
		"cache_read_tokens":     req.Detail.CacheReadTokens,
		"cache_creation_tokens": req.Detail.CacheCreationTokens,
		"response_header_keys":  sortedKeys(req.ResponseHeaders),
	})
	return okEnvelope(struct{}{})
}

// capture freezes the first raw payload seen for a method so the wire structs
// can be checked against real host bytes. It writes only when the file does
// not exist yet, so the earliest call wins.
//
// Only header credentials are redacted; request and response bodies are
// written verbatim, so a capture is safe to keep in the repository only for a
// run whose traffic is fixture traffic.
//
// plugin.reconfigure carries the same LifecycleRequest shape as
// plugin.register, so one capture covers both.
func capture(method string, payload []byte) {
	if captureDir == "" || len(payload) == 0 || method == runtime.MethodPluginReconfigure {
		return
	}
	path := filepath.Join(captureDir, method+".json")
	stateMu.Lock()
	defer stateMu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(redactAuthorization(payload))
}

// redactAuthorization blanks credential-bearing header values so a captured
// payload is safe to keep in the repository.
func redactAuthorization(payload []byte) []byte {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(payload, &generic); err != nil {
		return payload
	}
	redactObject(generic)
	if _, ok := generic["APIKey"]; ok {
		generic["APIKey"] = json.RawMessage(`"REDACTED"`)
	}
	// Options nests the headers the scheduler pick receives.
	if raw, ok := generic["Options"]; ok {
		options := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &options); err == nil {
			redactObject(options)
			if encoded, err := json.Marshal(options); err == nil {
				generic["Options"] = encoded
			}
		}
	}
	if scrubbed, err := json.Marshal(generic); err == nil {
		return scrubbed
	}
	return payload
}

func redactObject(object map[string]json.RawMessage) {
	for _, key := range []string{"Headers", "headers", "ResponseHeaders"} {
		raw, ok := object[key]
		if !ok {
			continue
		}
		headers := map[string][]string{}
		if err := json.Unmarshal(raw, &headers); err != nil {
			continue
		}
		for name := range headers {
			if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "X-Api-Key") {
				headers[name] = []string{"REDACTED"}
			}
		}
		if encoded, err := json.Marshal(headers); err == nil {
			object[key] = encoded
		}
	}
}

// record appends one observation as a JSON line. A failure here is silent:
// the probe must never fail a host call over its own bookkeeping.
func record(entry map[string]any) {
	if stateFile == "" {
		return
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	file, err := os.OpenFile(stateFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(raw, '\n'))
}

func hostLog(message string, fields map[string]any) {
	payload, err := json.Marshal(runtime.HostLogRequest{
		Level:   logLevel,
		Message: message,
		Fields:  fields,
	})
	if err != nil {
		return
	}
	_, _ = callHost(runtime.MethodHostLog, payload)
}

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

// firstValue looks a key up case-insensitively, so a host that delivers a
// non-canonical header key still counts as a hit.
func firstValue[M ~map[string][]string](in M, name string) string {
	for key, values := range in {
		if len(values) == 0 || !strings.EqualFold(key, name) {
			continue
		}
		return values[0]
	}
	return ""
}

// sortedKeys gives an observation a stable key list whatever the map's value
// type, so two runs of the same request produce comparable records. The empty
// result is an empty list rather than nil, so an observation of a map with no
// keys records [] instead of null.
func sortedKeys[M ~map[string]V, V any](in M) []string {
	out := make([]string, 0, len(in))
	return append(out, slices.Sorted(maps.Keys(in))...)
}

func okEnvelope(result any) ([]byte, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return json.Marshal(runtime.Envelope{OK: true, Result: encoded})
}

func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(runtime.Envelope{
		OK:    false,
		Error: &runtime.EnvelopeError{Code: code, Message: message},
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
