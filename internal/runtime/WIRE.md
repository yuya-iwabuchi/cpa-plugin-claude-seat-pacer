# Wire contract verification

Host: CLIProxyAPI v7.2.149 (tag `v7.2.149`, commit `2a6b87ac`), built from
source by the test.

The deployment target is 7.2.145 or newer. Everything below holds at 7.2.149;
nothing here establishes that lower bound.

## Verdict: header bridge CONFIRMED

A header the probe plugin injects in `request.intercept_before` arrives in
`SchedulerOptions.Headers` at the same plugin's `scheduler.pick`.

Evidence: `TestHeaderBridgeSurvivesToSchedulerPick` drives a real host process
with the probe under `testdata/probe`. The probe sets
`X-Cpa-Probe-Marker: probe-<n>-<request id>` in `intercept_before` and records
the marker it sees at `pick`; the test pairs the two through the request id the
marker embeds and fails unless they match. The captured
`testdata/host-payloads/scheduler.pick.json` shows the marker under
`Options.Headers` alongside the client's own headers. The probe always answers
`Handled: false`, so it never changes routing.

## Re-run

The host source is not vendored, so the test skips unless `CPA_SOURCE_DIR`
points at a CLIProxyAPI checkout:

```sh
CPA_SOURCE_DIR=~/dev/.research-cpa/CLIProxyAPI GOTOOLCHAIN=auto \
  go test ./internal/runtime -run HeaderBridge -v -count=1
```

`GOTOOLCHAIN=auto` lets Go fetch the toolchain the host's `go.mod` asks for.
Adding `CPA_CAPTURE_DIR=$PWD/internal/runtime/testdata/host-payloads` rewrites
the captured payloads from the live run. This is the command the source
comments in `wire_test.go` and `wire_probe_test.go` point at.

`wire_test.go` decodes every file in `testdata/host-payloads`, so a host that
changes a field name fails the unit tests without the host build.
`plugin.reconfigure` is not captured: it carries the same `LifecycleRequest`
shape as `plugin.register`, so the one capture covers both.

Captures are safe to keep in the repository only because the fixture run is
fixture traffic. The probe redacts `Authorization`, `X-Api-Key` and `APIKey`,
and nothing else — request and response bodies are written verbatim. They also
embed the run's temporary auth-file paths and the fixture proxy's port; no test
reads those.

## Confirmed by the live run

- Request shapes for `plugin.register`, `plugin.reconfigure`,
  `request.intercept_before`, `request.intercept_after`, `scheduler.pick`,
  `usage.handle`, `management.register`, `management.handle`.
- The host accepts `RegisterResult`, `RequestInterceptResponse.Headers`,
  `SchedulerPickResponse` with `Handled: false`,
  `ManagementRegistrationResponse` with `Routes` and `ManagementResponse` as
  encoded here.
- Host callbacks `host.log`, `host.auth.list`, `host.auth.get` and
  `host.http.do` round-trip with the request casing in `wire.go`.
  `host.auth.get` returns the credential file whole, access and refresh tokens
  included; the probe records its key names only.

## Source-derived only

- `SchedulerPickResponse` with `Handled: true`, `AuthID` or `DelegateBuiltin`
  set: the probe declines every pick, so an accepted pick is unexercised, and
  the two `SchedulerBuiltin*` values are never sent.
- `UsageRecord.ResponseHeaders` and non-zero `Detail` counters: the test
  refuses upstream traffic, so its capture is a failed record with zero
  tokens and null headers.
- `RequestInterceptResponse.Body`, `ClearHeaders`, `Terminate` and the
  `StatusCode`/`ResponseHeaders`/`ResponseBody` that go with it.
- `ManagementRegistrationResponse.Resources`, so `ResourceRoute` in full: its
  exact-match rule, its rejected paths, and `ManagementRoute.Menu` demoting a
  GET route to an unauthenticated resource.
- `Metadata.Logo`, and `Metadata.ConfigFields` beyond the empty list the probe
  sends.
- `SchedulerAuthCandidate.Metadata` contents; the capture shows `null`.
- `LifecycleRequest.ConfigYAML` carrying `enabled` and `priority` the config
  file omits: the fixture config writes both, so the capture cannot show the
  host adding them.
- The plugin ID the host derives from the library filename, including the
  `-v<version>` suffix it strips: the probe's filename carries no version.
- `SchedulerOptions.Metadata["pinned_auth_id"]`, absent from every capture.
- Every `HostAuthFileEntry` field except `id`, `auth_index`, `name`, `type`,
  `provider` and `status`, which are the ones the probe's report reads back.
