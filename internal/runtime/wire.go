// Package runtime holds the plugin's wire types, hook handlers, and lifecycle.
//
// The JSON in this file is the contract with CLIProxyAPI, derived from host
// v7.2.149 (tag v7.2.149, commit 2a6b87ac) and checked against a running host
// by TestHeaderBridgeSurvivesToSchedulerPick. WIRE.md records what that run
// proved and what is still source-derived only.
//
// CASING IS MIXED AND LOAD-BEARING. Two conventions travel over the same
// boundary and every struct below states which one it uses:
//
//   - snake_case for the RPC envelopes the host declares itself, in
//     internal/pluginhost/rpc_schema.go, internal/pluginhost/host_callbacks.go
//     and internal/pluginhost/auth_callbacks.go.
//   - PascalCase for payloads modelled on sdk/pluginapi, which are untagged Go
//     structs on the host side and so serialize under their field names.
//
// Several messages mix both: an outer snake_case envelope wrapping PascalCase
// members. Do not "normalise" these tags.
package runtime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// ABIVersion is the native C ABI shape the plugin library implements.
// Source: sdk/pluginabi/types.go:7.
const ABIVersion uint32 = 1

// SchemaVersion is the RPC contract this plugin declares at registration.
// The host accepts any value up to its own (4 in v7.2.149) and treats a
// higher one as unsupported; declaring 1 keeps the plugin loadable on older
// hosts, because the host refuses a plugin whose schema exceeds its own.
// Source: sdk/pluginabi/types.go:14, internal/pluginhost/rpc_client.go:69.
const SchemaVersion uint32 = 1

// Plugin methods the host calls on this library.
// Source: sdk/pluginabi/types.go:23-91.
const (
	MethodPluginRegister         = "plugin.register"
	MethodPluginReconfigure      = "plugin.reconfigure"
	MethodPluginQuiesce          = "plugin.quiesce"
	MethodPluginShutdown         = "plugin.shutdown"
	MethodRequestInterceptBefore = "request.intercept_before"
	MethodRequestInterceptAfter  = "request.intercept_after"
	MethodSchedulerPick          = "scheduler.pick"
	MethodUsageHandle            = "usage.handle"
	MethodManagementRegister     = "management.register"
	MethodManagementHandle       = "management.handle"
)

// Host callbacks this plugin calls back into the host.
// Source: sdk/pluginabi/types.go:80-91.
const (
	MethodHostHTTPDo   = "host.http.do"
	MethodHostLog      = "host.log"
	MethodHostAuthList = "host.auth.list"
	MethodHostAuthGet  = "host.auth.get"
)

// Built-in schedulers a pick may delegate to.
// Source: sdk/pluginapi/types.go:461-466.
const (
	SchedulerBuiltinRoundRobin = "round-robin"
	SchedulerBuiltinFillFirst  = "fill-first"
)

// Capability keys in the registration response. The host decodes them into a
// snake_case struct, so an unknown key is silently dropped rather than
// rejected. The full set is at internal/pluginhost/rpc_schema.go:21.
const (
	CapabilityRequestInterceptor = "request_interceptor"
	CapabilityScheduler          = "scheduler"
	CapabilityUsagePlugin        = "usage_plugin"
	CapabilityManagementAPI      = "management_api"
)

// Keys observed in SchedulerOptions.Metadata and RequestInterceptRequest.Metadata.
// The host declares them in sdk/cliproxy/executor/types.go:12-78. They are
// host-owned and best-effort: treat every one as optional.
const (
	// MetadataRequestedModel is the client-requested model before aliasing.
	MetadataRequestedModel = "requested_model"
	// MetadataRequestPath is the inbound HTTP path, absent for non-HTTP execution.
	MetadataRequestPath = "request_path"
	// MetadataDerivedSessionID is a session identity the host infers from
	// request context. It is not the Claude Code session id, which lives in
	// the request body and is only visible to request.intercept_before.
	MetadataDerivedSessionID = "derived_session_id"
	// MetadataCallerScope isolates inferred session identities per downstream caller.
	MetadataCallerScope = "caller_scope"
	// MetadataSelectedAuthID names the credential a previous attempt used, so
	// it is present only on a retry pick.
	MetadataSelectedAuthID = "selected_auth_id"
	// MetadataSessionAffinityProvider is the affinity namespace, literally
	// "mixed" for a multi-provider route.
	MetadataSessionAffinityProvider = "session_affinity_provider"
	// MetadataPinnedAuthID locks execution to one credential. When set, the
	// host offers exactly that credential as the only candidate.
	MetadataPinnedAuthID = "pinned_auth_id"
)

// Envelope wraps every call in both directions. Casing: snake_case.
// Source: sdk/pluginabi/types.go:95.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError carries a machine code and human message for a failed call.
// Casing: snake_case. Source: sdk/pluginabi/types.go:101.
type EnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Retryable and HTTPStatus are host-to-plugin only; the host ignores them
	// on a plugin response.
	Retryable  bool `json:"retryable,omitempty"`
	HTTPStatus int  `json:"http_status,omitempty"`
}

// LifecycleRequest is the plugin.register and plugin.reconfigure payload.
// Casing: snake_case. Source: internal/pluginhost/rpc_schema.go:10.
//
// ConfigYAML is the plugin's own block under plugins.configs.<plugin-id>,
// re-serialized as YAML and carried as base64 because Go encodes []byte that
// way. The host injects enabled and priority into it even when the file omits
// them. SchemaVersion is the host's contract version, not the plugin's.
//
// plugin.reconfigure fires repeatedly during startup and on every config
// reload, so a handler must be idempotent and cheap.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// RegisterResult answers plugin.register and plugin.reconfigure.
// Casing: MIXED. The three outer keys are snake_case, Metadata's members are
// PascalCase, and Capabilities' keys are snake_case.
// Source: internal/pluginhost/rpc_schema.go:15.
type RegisterResult struct {
	SchemaVersion uint32          `json:"schema_version"`
	Metadata      Metadata        `json:"metadata"`
	Capabilities  map[string]bool `json:"capabilities"`
}

// Metadata is the plugin's self-description. The host rejects the plugin when
// Name, Version, Author, or GitHubRepository is blank. The plugin ID is not
// declared here: the host derives it from the library filename, minus the
// extension and an optional -v<version> suffix.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:24,
// internal/pluginhost/platform.go:47.
type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

// ConfigField describes a plugin-owned configuration key so the management UI
// renders an input for it. Type is one of string, number, integer, boolean,
// enum, array, object.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:60, :42-57.
type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description,omitempty"`
}

// RequestInterceptRequest is the request.intercept_before and
// request.intercept_after payload.
//
// Casing: MIXED. Every member is PascalCase except host_callback_id, which the
// host adds around the embedded pluginapi struct. Passing that ID back on a
// host callback ties the callback to this request's context.
//
// Source: sdk/pluginapi/types.go:995, internal/pluginhost/rpc_schema.go:89.
//
// This is the only hook that sees Body, which is why conversation identity has
// to be derived here rather than at the pick.
type RequestInterceptRequest struct {
	// RequestID identifies one model execution and matches request.complete.
	RequestID string `json:"RequestID"`
	// TraceID identifies the parent inbound HTTP request when available.
	TraceID string `json:"TraceID"`
	// SourceFormat is the original client protocol format.
	SourceFormat string `json:"SourceFormat"`
	// ToFormat is the selected upstream protocol format. It is empty before
	// credential selection, so it distinguishes the before hook from the after
	// hook when both share a handler.
	ToFormat string `json:"ToFormat"`
	// Model is the current execution model, upstream-resolved after selection.
	Model string `json:"Model"`
	// RequestedModel is the client-requested model before alias rewriting.
	RequestedModel string `json:"RequestedModel"`
	// Stream reports whether the request expects streaming output.
	Stream bool `json:"Stream"`
	// Headers are the current upstream request headers.
	Headers http.Header `json:"Headers"`
	// Body is the current request payload, base64-encoded on the wire.
	Body []byte `json:"Body"`
	// Metadata is a best-effort context snapshot. It is read-only and its keys
	// are host-owned; see the Metadata* constants.
	Metadata map[string]any `json:"Metadata"`

	// HostCallbackID scopes host callbacks to this request.
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// RequestInterceptResponse returns request modifications.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:1019.
//
// There is no metadata field: a value the interceptor wants the pick to see
// has to travel as a header, which the host merges into the live headers
// (internal/pluginhost/adapters_interceptors.go:127) and then writes onto
// opts.Headers (sdk/api/handlers/handlers_interceptors.go:464), from where the
// pick receives a clone (sdk/cliproxy/auth/conductor_selection.go:729).
//
// Headers replaces the named headers and leaves the rest alone; ClearHeaders
// removes headers before Headers is applied; Body replaces the payload only
// when non-empty. Header keys are canonicalized by the host's merge, so a key
// sent in non-canonical form arrives canonicalized downstream.
type RequestInterceptResponse struct {
	Headers      http.Header `json:"Headers,omitempty"`
	Body         []byte      `json:"Body,omitempty"`
	ClearHeaders []string    `json:"ClearHeaders,omitempty"`
	// Terminate stops the interceptor chain and fails the request downstream
	// without reaching an upstream executor.
	Terminate bool `json:"Terminate,omitempty"`
	// StatusCode is the downstream status when Terminate is true. An invalid
	// value defaults to 403.
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

// SchedulerPickRequest is the scheduler.pick payload.
// Casing: PascalCase throughout, with no host_callback_id: the host sends the
// bare pluginapi struct.
// Source: sdk/pluginapi/types.go:480, internal/pluginhost/rpc_client.go:381,
// sdk/cliproxy/auth/conductor_selection.go:798.
//
// Provider is empty for a multi-provider route, which is what an inbound
// /v1/messages produces; Providers is the field to read. Candidates are
// already filtered to the eligible, untried, highest-priority tier, so the
// plugin never sees a disabled, cooling, or model-incompatible credential.
type SchedulerPickRequest struct {
	// Plugin is this plugin's own metadata, filled in by the host.
	Plugin Metadata `json:"Plugin"`
	// Provider is the primary provider key, empty on a mixed route.
	Provider string `json:"Provider"`
	// Providers lists every provider key the route accepts.
	Providers []string `json:"Providers"`
	// Model is the requested model identifier.
	Model string `json:"Model"`
	// Stream reports whether the request expects streaming output.
	Stream bool `json:"Stream"`
	// Options carries request-scoped scheduler inputs.
	Options SchedulerOptions `json:"Options"`
	// Candidates are the auth records available for selection.
	Candidates []SchedulerAuthCandidate `json:"Candidates"`
}

// SchedulerOptions carries request-scoped scheduler inputs.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:498.
//
// Headers is a verbatim clone of the upstream headers as they stand after the
// before-auth interceptors have run, which is what carries an interceptor's
// injected header into the pick. Metadata holds host-owned keys only; see the
// Metadata* constants.
type SchedulerOptions struct {
	Headers  map[string][]string `json:"Headers"`
	Metadata map[string]any      `json:"Metadata"`
}

// SchedulerAuthCandidate describes one credential offered to the pick.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:506.
//
// ID is the auth record id, which for file-backed credentials is the auth file
// name. Attributes carries immutable routing attributes with sensitive keys
// removed. Metadata is declared but the selection path never populates it
// (sdk/cliproxy/auth/conductor_selection.go:697), so it always arrives nil.
type SchedulerAuthCandidate struct {
	ID         string            `json:"ID"`
	Provider   string            `json:"Provider"`
	Priority   int               `json:"Priority"`
	Status     string            `json:"Status"`
	Attributes map[string]string `json:"Attributes"`
	Metadata   map[string]any    `json:"Metadata"`
}

// SchedulerPickResponse returns the routing decision.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:522.
//
// Handled false declines and hands the choice to the host's own selector; it
// is the only answer that cannot change routing. With Handled true, AuthID
// must name a candidate from the request or DelegateBuiltin must name a
// built-in scheduler, or the host logs the response as invalid and falls back
// (internal/pluginhost/scheduler.go:26). Returning an error envelope instead
// hard-fails the request with no fallback
// (sdk/cliproxy/auth/conductor_selection.go:807).
type SchedulerPickResponse struct {
	AuthID          string `json:"AuthID,omitempty"`
	DelegateBuiltin string `json:"DelegateBuiltin,omitempty"`
	Handled         bool   `json:"Handled"`
}

// UsageRecord is the usage.handle payload. The host expects an empty result
// object back and only debug-logs a failure.
// Casing: PascalCase, with no host_callback_id.
// Source: sdk/pluginapi/types.go:1341, internal/pluginhost/rpc_client.go:561.
//
// Latency and TTFT are time.Duration and therefore arrive as integer
// nanoseconds. ResponseHeaders is populated only for a request that received
// an upstream HTTP response, so a transport-level failure delivers it empty.
type UsageRecord struct {
	Provider     string `json:"Provider"`
	ExecutorType string `json:"ExecutorType"`
	Model        string `json:"Model"`
	Alias        string `json:"Alias"`
	APIKey       string `json:"APIKey"`
	// AuthID identifies the credential used, matching SchedulerAuthCandidate.ID.
	AuthID string `json:"AuthID"`
	// AuthIndex is the stable runtime credential index, the key host.auth.get
	// takes.
	AuthIndex       string        `json:"AuthIndex"`
	AuthType        string        `json:"AuthType"`
	Source          string        `json:"Source"`
	ReasoningEffort string        `json:"ReasoningEffort"`
	ServiceTier     string        `json:"ServiceTier"`
	Generate        bool          `json:"Generate"`
	RequestedAt     time.Time     `json:"RequestedAt"`
	Latency         time.Duration `json:"Latency"`
	TTFT            time.Duration `json:"TTFT"`
	Failed          bool          `json:"Failed"`
	Failure         UsageFailure  `json:"Failure"`
	Detail          UsageDetail   `json:"Detail"`
	// ResponseHeaders carries the upstream rate-limit headers this plugin
	// reads for free quota observation.
	ResponseHeaders http.Header `json:"ResponseHeaders"`
}

// UsageFailure describes an upstream or executor failure.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:1384.
//
// StatusCode is zero when the request failed before an HTTP status existed,
// so Failed is the reliable signal and StatusCode is a refinement.
type UsageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

// UsageDetail contains token accounting counters.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:1392.
//
// CachedTokens is a provider-reported total and is not necessarily
// CacheReadTokens plus CacheCreationTokens; the two split counters are the
// ones this plugin measures cache effectiveness with.
type UsageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

// ManagementRegistrationRequest is the management.register payload.
// Casing: PascalCase, with no host_callback_id.
// Source: sdk/pluginapi/types.go:1268, internal/pluginhost/rpc_client.go:575.
//
// management.register is re-issued on every plugin.reconfigure, so a handler
// must return the same routes each time.
type ManagementRegistrationRequest struct {
	Plugin           Metadata `json:"Plugin"`
	BasePath         string   `json:"BasePath"`
	ResourceBasePath string   `json:"ResourceBasePath"`
}

// ManagementRegistrationResponse answers management.register.
// Casing: MIXED. The two outer keys are lowercase and the route members are
// PascalCase. Source: internal/pluginhost/rpc_schema.go:129.
type ManagementRegistrationResponse struct {
	Routes    []ManagementRoute `json:"routes,omitempty"`
	Resources []ResourceRoute   `json:"resources,omitempty"`
}

// ManagementRoute is an authenticated JSON endpoint. Path is resolved under
// /v0/management/. The host owns the Handler field on its own struct and fills
// it in itself, so the plugin must not send one.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:1286,
// internal/pluginhost/rpc_client.go:582.
type ManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

// ResourceRoute is a browser page under /v0/resource/plugins/<plugin-id>/.
// Resource requests are NOT management-authenticated, so a handler must never
// emit a credential into its response.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:1300.
type ResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description,omitempty"`
}

// ManagementRequest is the management.handle payload.
// Casing: MIXED. Every member is PascalCase except host_callback_id.
// Source: sdk/pluginapi/types.go:1317, internal/pluginhost/rpc_schema.go:124.
//
// Path is the full absolute request path, not the registered suffix.
type ManagementRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`

	// HostCallbackID scopes host callbacks to this request.
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// ManagementResponse answers management.handle. StatusCode zero means 200.
// Casing: PascalCase. Source: sdk/pluginapi/types.go:1331.
type ManagementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

// HostHTTPRequest is the host.http.do payload. Routing through the host
// applies the host's proxy and TLS settings.
// Casing: snake_case. Source: internal/pluginhost/host_callbacks.go:17.
type HostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

// HostHTTPResponse is the host.http.do result member.
// Casing: PascalCase, because the host returns the untagged pluginapi struct.
// Source: sdk/pluginapi/types.go:790, internal/pluginhost/host_callbacks.go:142.
type HostHTTPResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

// HostAuthListRequest is the host.auth.list payload. The host decodes it as an
// arbitrary object and reads nothing from it, so an empty object is valid.
// Casing: snake_case. Source: internal/pluginhost/auth_callbacks.go:53.
type HostAuthListRequest struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// HostAuthListResponse is the host.auth.list result member.
// Casing: snake_case. Source: internal/pluginhost/auth_callbacks.go:23.
type HostAuthListResponse struct {
	Files []HostAuthFileEntry `json:"files"`
}

// HostAuthFileEntry describes one credential. Only the fields this plugin
// reads are modelled; the full set is at sdk/pluginapi/types.go:676.
// Casing: snake_case, uniquely among the pluginapi structs, because this one
// carries explicit tags on the host side.
//
// ID is the value that matches SchedulerAuthCandidate.ID and UsageRecord.AuthID.
// AuthIndex is the separate key host.auth.get takes.
type HostAuthFileEntry struct {
	ID            string    `json:"id,omitempty"`
	AuthIndex     string    `json:"auth_index,omitempty"`
	Name          string    `json:"name"`
	Type          string    `json:"type,omitempty"`
	Provider      string    `json:"provider,omitempty"`
	Label         string    `json:"label,omitempty"`
	Status        string    `json:"status,omitempty"`
	StatusMessage string    `json:"status_message,omitempty"`
	Disabled      bool      `json:"disabled,omitempty"`
	Unavailable   bool      `json:"unavailable,omitempty"`
	RuntimeOnly   bool      `json:"runtime_only,omitempty"`
	Email         string    `json:"email,omitempty"`
	AccountType   string    `json:"account_type,omitempty"`
	Priority      int       `json:"priority,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
	LastRefresh   time.Time `json:"last_refresh,omitempty"`
}

// HostAuthGetRequest asks for credential JSON by auth index. The host rejects
// a blank AuthIndex.
// Casing: snake_case. Source: internal/pluginhost/auth_callbacks.go:19.
type HostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

// HostAuthGetResponse is the host.auth.get result member.
// Casing: snake_case. Source: internal/pluginhost/auth_callbacks.go:27.
//
// JSON is the raw credential file, access and refresh tokens included. Never
// log it, and read only the field needed to call the provider.
type HostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

// HostLogRequest is the host.log payload. Level is one of trace, info, warn,
// warning, or error; anything else logs at debug.
// Casing: snake_case. Source: internal/pluginhost/host_callbacks.go:57, :326.
type HostLogRequest struct {
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	Level          string         `json:"level,omitempty"`
	Message        string         `json:"message,omitempty"`
	Fields         map[string]any `json:"fields,omitempty"`
}
