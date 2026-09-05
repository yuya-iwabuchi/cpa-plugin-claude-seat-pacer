package runtime

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/host-payloads holds request payloads captured from CLIProxyAPI
// v7.2.149 by the probe. Decoding them here fails the moment a tag in wire.go
// is "normalised" away from the casing the host actually sends. Every capture
// in that directory is decoded by a test in this file. WIRE.md carries the
// command that refreshes them.
func hostPayload(t *testing.T, method string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "host-payloads", method+".json"))
	if err != nil {
		t.Fatalf("read captured %s payload: %v", method, err)
	}
	return raw
}

func TestLifecycleRequestDecodesHostPayload(t *testing.T) {
	var req LifecycleRequest
	if err := json.Unmarshal(hostPayload(t, MethodPluginRegister), &req); err != nil {
		t.Fatal(err)
	}
	if req.SchemaVersion == 0 {
		t.Error("schema_version did not decode")
	}
	// config_yaml travels base64-encoded and decodes to the plugin's own YAML
	// block.
	if got := string(req.ConfigYAML); got == "" {
		t.Fatal("config_yaml did not decode")
	} else if want := "enabled: true"; !strings.Contains(got, want) {
		t.Errorf("config_yaml = %q, want it to contain %q", got, want)
	}
}

func TestRequestInterceptRequestDecodesHostPayload(t *testing.T) {
	var req RequestInterceptRequest
	if err := json.Unmarshal(hostPayload(t, MethodRequestInterceptBefore), &req); err != nil {
		t.Fatal(err)
	}
	if req.RequestID == "" {
		t.Error("RequestID did not decode")
	}
	if req.Model == "" {
		t.Error("Model did not decode")
	}
	// The body is the only place the Claude Code session id appears, so a
	// tag change that drops it would silently disable conversation affinity.
	if len(req.Body) == 0 || req.Body[0] != '{' {
		t.Errorf("Body did not decode as JSON: %q", req.Body)
	}
	if len(req.Headers) == 0 {
		t.Error("Headers did not decode")
	}
	// The before hook runs ahead of credential selection.
	if req.ToFormat != "" {
		t.Errorf("ToFormat = %q, want empty before auth", req.ToFormat)
	}
	if req.HostCallbackID == "" {
		t.Error("host_callback_id did not decode; host callbacks would lose request scope")
	}
}

func TestRequestInterceptAfterDecodesHostPayload(t *testing.T) {
	var req RequestInterceptRequest
	if err := json.Unmarshal(hostPayload(t, MethodRequestInterceptAfter), &req); err != nil {
		t.Fatal(err)
	}
	// The after hook runs once the upstream format is settled, which is what
	// separates it from the before hook when one handler serves both.
	if req.ToFormat == "" {
		t.Error("ToFormat = empty, want the resolved upstream format after auth")
	}
	// The header an interceptor injected before auth is still on the request.
	if req.Headers.Get("X-Cpa-Probe-Marker") == "" {
		t.Errorf("injected marker is absent from Headers: %v", req.Headers)
	}
	if _, ok := req.Metadata[MetadataSelectedAuthID]; !ok {
		t.Errorf("Metadata has no %s: %v", MetadataSelectedAuthID, req.Metadata)
	}
}

func TestSchedulerPickRequestDecodesHostPayload(t *testing.T) {
	var req SchedulerPickRequest
	if err := json.Unmarshal(hostPayload(t, MethodSchedulerPick), &req); err != nil {
		t.Fatal(err)
	}
	// An inbound /v1/messages is a mixed route, so Provider is empty and
	// Providers is the field that names the provider.
	if req.Provider != "" {
		t.Errorf("Provider = %q, want empty on a mixed route", req.Provider)
	}
	if len(req.Providers) == 0 {
		t.Error("Providers did not decode")
	}
	if len(req.Candidates) == 0 {
		t.Fatal("Candidates did not decode")
	}
	if req.Candidates[0].ID == "" {
		t.Error("candidate ID did not decode")
	}
	if req.Candidates[0].Status == "" {
		t.Error("candidate Status did not decode")
	}
	if len(req.Candidates[0].Attributes) == 0 {
		t.Error("candidate Attributes did not decode")
	}
	// The host never fills candidate metadata on this path.
	if req.Candidates[0].Metadata != nil {
		t.Errorf("candidate Metadata = %v, want nil", req.Candidates[0].Metadata)
	}
	if req.Plugin.Name == "" {
		t.Error("Plugin metadata did not decode")
	}
	// The header bridge: what an interceptor injected arrives here.
	if len(req.Options.Headers) == 0 {
		t.Fatal("Options.Headers did not decode")
	}
	if _, ok := req.Options.Metadata[MetadataRequestPath]; !ok {
		t.Errorf("Options.Metadata has no %s: %v", MetadataRequestPath, req.Options.Metadata)
	}
	if _, ok := req.Options.Metadata[MetadataDerivedSessionID]; !ok {
		t.Errorf("Options.Metadata has no %s: %v", MetadataDerivedSessionID, req.Options.Metadata)
	}
}

func TestUsageRecordDecodesHostPayload(t *testing.T) {
	var record UsageRecord
	if err := json.Unmarshal(hostPayload(t, MethodUsageHandle), &record); err != nil {
		t.Fatal(err)
	}
	if record.AuthID == "" {
		t.Error("AuthID did not decode")
	}
	if record.AuthIndex == "" {
		t.Error("AuthIndex did not decode")
	}
	if record.Model == "" {
		t.Error("Model did not decode")
	}
	// The captured request failed at the transport, before a status existed.
	if !record.Failed {
		t.Error("Failed did not decode")
	}
	if record.Failure.Body == "" {
		t.Error("Failure.Body did not decode")
	}
	if record.RequestedAt.IsZero() {
		t.Error("RequestedAt did not decode")
	}
	if record.Latency <= 0 {
		t.Errorf("Latency = %v, want a positive duration", record.Latency)
	}
}

func TestManagementRequestDecodesHostPayload(t *testing.T) {
	var req ManagementRequest
	if err := json.Unmarshal(hostPayload(t, MethodManagementHandle), &req); err != nil {
		t.Fatal(err)
	}
	if req.Method == "" {
		t.Error("Method did not decode")
	}
	// Path is absolute, not the registered suffix.
	if len(req.Path) == 0 || req.Path[0] != '/' {
		t.Errorf("Path = %q, want an absolute path", req.Path)
	}
	if len(req.Query) == 0 {
		t.Error("Query did not decode")
	}
	if req.HostCallbackID == "" {
		t.Error("host_callback_id did not decode")
	}

	var registration ManagementRegistrationRequest
	if err := json.Unmarshal(hostPayload(t, MethodManagementRegister), &registration); err != nil {
		t.Fatal(err)
	}
	if registration.BasePath == "" || registration.ResourceBasePath == "" {
		t.Errorf("registration paths did not decode: %+v", registration)
	}
	// The host echoes the plugin's own metadata back on this hook.
	if registration.Plugin.Name == "" || registration.Plugin.GitHubRepository == "" {
		t.Errorf("registration plugin metadata did not decode: %+v", registration.Plugin)
	}
}

// TestResponseEncodingUsesHostKeys keeps the exact keys the host decodes. The
// host reads registration capabilities and route lists under lowercase keys
// while their members stay PascalCase, so this asserts both halves of that
// split.
func TestResponseEncodingUsesHostKeys(t *testing.T) {
	registerJSON, err := json.Marshal(RegisterResult{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name: "n", Version: "v", Author: "a", GitHubRepository: "r",
			Logo:         "data:image/svg+xml;base64,PC9zdmc+",
			ConfigFields: []ConfigField{{Name: "spread", Type: "enum", EnumValues: []string{"off"}, Description: "d"}},
		},
		Capabilities: map[string]bool{CapabilityScheduler: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"schema_version"`, `"metadata"`, `"capabilities"`, `"Name"`, `"GitHubRepository"`, `"scheduler"`,
		`"Logo"`, `"ConfigFields"`, `"Type"`, `"EnumValues"`, `"Description"`,
	} {
		if !strings.Contains(string(registerJSON), want) {
			t.Errorf("register result %s is missing %s", registerJSON, want)
		}
	}

	// ManagementRoute and ResourceRoute share three member names, so the two
	// lists are compared whole rather than searched for keys.
	routesJSON, err := json.Marshal(ManagementRegistrationResponse{
		Routes:    []ManagementRoute{{Method: http.MethodGet, Path: "/api/status", Description: "d"}},
		Resources: []ResourceRoute{{Path: "/index.html", Menu: "Quota", Description: "d"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantRoutes = `{"routes":[{"Method":"GET","Path":"/api/status","Description":"d"}],` +
		`"resources":[{"Path":"/index.html","Menu":"Quota","Description":"d"}]}`
	if string(routesJSON) != wantRoutes {
		t.Errorf("management registration = %s, want %s", routesJSON, wantRoutes)
	}
	// Both lists drop out when empty, so a plugin with no resources does not
	// send an empty one.
	emptyRoutesJSON, err := json.Marshal(ManagementRegistrationResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if string(emptyRoutesJSON) != `{}` {
		t.Errorf("empty management registration = %s", emptyRoutesJSON)
	}

	pickJSON, err := json.Marshal(SchedulerPickResponse{Handled: false})
	if err != nil {
		t.Fatal(err)
	}
	if string(pickJSON) != `{"Handled":false}` {
		t.Errorf("decline pick = %s, want {\"Handled\":false}", pickJSON)
	}

	interceptJSON, err := json.Marshal(RequestInterceptResponse{
		Headers: map[string][]string{"X-Test": {"1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(interceptJSON) != `{"Headers":{"X-Test":["1"]}}` {
		t.Errorf("intercept response = %s", interceptJSON)
	}

	// StatusCode has no omitempty because the host reads a zero as 200, so an
	// explicit zero and an absent key have to encode the same way.
	managementJSON, err := json.Marshal(ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(managementJSON) != `{"StatusCode":200,"Headers":{"Content-Type":["application/json"]},"Body":"e30="}` {
		t.Errorf("management response = %s", managementJSON)
	}
	emptyManagementJSON, err := json.Marshal(ManagementResponse{})
	if err != nil {
		t.Fatal(err)
	}
	if string(emptyManagementJSON) != `{"StatusCode":0}` {
		t.Errorf("empty management response = %s", emptyManagementJSON)
	}

	// Every result above travels inside this envelope, in both directions.
	okJSON, err := json.Marshal(Envelope{OK: true, Result: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(okJSON) != `{"ok":true,"result":{}}` {
		t.Errorf("ok envelope = %s", okJSON)
	}
	failedJSON, err := json.Marshal(Envelope{Error: &EnvelopeError{Code: "c", Message: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(failedJSON) != `{"ok":false,"error":{"code":"c","message":"m"}}` {
		t.Errorf("error envelope = %s", failedJSON)
	}
}

func TestHostCallbackRequestsUseSnakeCase(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		want    []string
	}{
		{"host.http.do", HostHTTPRequest{Method: "GET", URL: "http://x", HostCallbackID: "1"},
			[]string{`"method"`, `"url"`, `"host_callback_id"`}},
		{"host.auth.get", HostAuthGetRequest{AuthIndex: "abc"}, []string{`"auth_index"`}},
		{"host.log", HostLogRequest{Level: "warn", Message: "m"}, []string{`"level"`, `"message"`}},
	}
	for _, testCase := range cases {
		raw, err := json.Marshal(testCase.payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range testCase.want {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s request %s is missing %s", testCase.name, raw, want)
			}
		}
	}

	// The auth callbacks answer in snake_case, unlike the PascalCase results
	// of host.http.do.
	var list HostAuthListResponse
	if err := json.Unmarshal([]byte(`{"files":[{"id":"a.json","auth_index":"ix","name":"a.json","provider":"claude","status":"active"}]}`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Files) != 1 || list.Files[0].AuthIndex != "ix" || list.Files[0].ID != "a.json" {
		t.Errorf("auth list decoded to %+v", list.Files)
	}

	var httpResponse HostHTTPResponse
	if err := json.Unmarshal([]byte(`{"StatusCode":502,"Headers":{"A":["b"]},"Body":"eHk="}`), &httpResponse); err != nil {
		t.Fatal(err)
	}
	if httpResponse.StatusCode != 502 || string(httpResponse.Body) != "xy" {
		t.Errorf("host http response decoded to %+v", httpResponse)
	}
}
