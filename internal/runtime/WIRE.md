# Wire contract verification

Host: CLIProxyAPI v7.2.149 (tag `v7.2.149`, commit `2a6b87ac`), built from
source by the test. Deployment target is 7.2.145 or newer.

## Verdict: header bridge CONFIRMED

A header the probe plugin injects in `request.intercept_before` arrives in
`SchedulerOptions.Headers` at the same plugin's `scheduler.pick`.

Evidence: `TestHeaderBridgeSurvivesToSchedulerPick` drives a real host process
with the probe under `testdata/probe`. The probe sets
`X-Cpa-Probe-Marker: probe-<n>-<request id>` in `intercept_before` and records
the marker it sees at `pick`; the test fails unless both values match. The
captured `testdata/host-payloads/scheduler.pick.json` shows the marker under
`Options.Headers` alongside the client's own headers. The probe always answers
`Handled: false`, so it never changes routing.

## Re-run

```sh
CPA_SOURCE_DIR=~/dev/.research-cpa/CLIProxyAPI GOTOOLCHAIN=auto \
  go test ./internal/runtime -run HeaderBridge -v -count=1
```

`GOTOOLCHAIN=auto` lets Go fetch the toolchain the host's `go.mod` asks for.
Setting `CPA_CAPTURE_DIR=internal/runtime/testdata/host-payloads` rewrites the
captured payloads from the live run; `wire_test.go` decodes every capture, so
a host that changes a field name fails the unit tests without the host build.

## Confirmed by the live run

- Request shapes for `plugin.register`, `plugin.reconfigure`,
  `request.intercept_before`, `request.intercept_after`, `scheduler.pick`,
  `usage.handle`, `management.register`, `management.handle` (one capture each).
- The host accepts `RegisterResult`, `RequestInterceptResponse.Headers`,
  `SchedulerPickResponse` with `Handled: false`,
  `ManagementRegistrationResponse` and `ManagementResponse` as encoded here.
- Host callbacks `host.log`, `host.auth.list`, `host.auth.get` and
  `host.http.do` round-trip with the request casing in `wire.go`.

## Source-derived only

- `SchedulerPickResponse` with `Handled: true`, `AuthID` or `DelegateBuiltin`
  set: the probe declines every pick, so an accepted pick is unexercised.
- `UsageRecord.ResponseHeaders` and non-zero `Detail` counters: the test
  refuses upstream traffic, so its capture is a failed record with zero
  tokens and null headers.
- `RequestInterceptResponse.Body` and `ClearHeaders`.
- `SchedulerAuthCandidate.Metadata` contents; the capture shows `null`.
