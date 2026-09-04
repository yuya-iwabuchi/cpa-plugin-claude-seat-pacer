# Agent brief

Load-bearing facts for anyone (human or agent) changing this plugin. Verified
against CLIProxyAPI v7.2.149 source; the deployment target is 7.2.145+.

## What this plugin is

A CLIProxyAPI plugin that routes requests across multiple Claude OAuth
subscription credentials by **burn pace** while keeping each conversation
pinned to one credential so Anthropic prompt caches keep hitting.

Declared capabilities: `request_interceptor`, `scheduler`, `usage_plugin`,
`management_api`.

## The two facts the whole design rests on

**1. Prompt caching is worth ~7x, and caches never cross an organization.**
Measured over 2,096 real requests: 96.5% of input-side tokens are cache reads,
and caching removes 86% of input-side cost. Anthropic documents that caches are
isolated between organizations, so moving a live conversation to a credential in
another org is a guaranteed full miss costing ~12.5x on that request. Stickiness
is therefore not a nicety — a router that ignores it loses more than it saves.

**2. `request.intercept_before` is the only way to learn the conversation id.**
`scheduler.pick` receives `Headers` and `Metadata` but **not the request body**,
and for Claude Code the session id lives in the body (`metadata.user_id`), not a
header. `request.intercept_before` does receive the full body, and header
mutations it returns survive into `opts.Headers`, which the host clones verbatim
into `SchedulerOptions.Headers`. So the interceptor derives the key and injects
a private header; the scheduler reads it back.

This header bridge is emergent behaviour, not a documented contract. The E2E
test asserts it round-trips. Do not remove that test.

## Host invariants that constrain the code

- **`scheduler.pick` has no timeout.** Context cancellation does not unwind the
  plugin. Any blocking call there parks a goroutine permanently and blocks
  `dlclose`. The pick reads an in-memory snapshot only; all fetching happens on
  a background goroutine.
- **One panic fuses the plugin permanently**, across every capability it
  declares, including the interceptor. `recover()` does not cross the C
  boundary, so every exported entry point installs its own.
- **`pick` is called concurrently with no serialization.** Config is held in an
  `atomic.Pointer`; mutable state is behind a mutex held only for map access.
- **Never return an error envelope from `scheduler.pick`.** It hard-fails the
  request with no fallback to the host's selector. `Handled: false` is the safe
  decline, and it is the default for anything the plugin is unsure about.
- **Candidates are pre-filtered and capped at the highest priority tier.**
  Disabled, cooling, model-incompatible and already-tried credentials are gone
  before the plugin is asked, and lower priority tiers are never offered. All
  credentials in the pool must share one `priority` value or spreading silently
  does nothing. Detect single-candidate lists and warn.
- **A plugin pick never seeds the host's affinity cache.** `SessionCache.Touch`
  refuses to create entries, so a hybrid where the host keeps affinity and the
  plugin only biases cold starts cannot bootstrap. This plugin owns affinity;
  run with `routing.session-affinity: false`.
- **Only one scheduler plugin is ever consulted** (first non-fused by priority).
  Mutually exclusive with other scheduler plugins.
- **Home mode bypasses the scheduler hook entirely.** Surface that rather than
  appearing to do nothing.
- **Declare `schema_version: 1`**, not the host's current value, or the plugin
  silently requires a host at least as new as its build SDK.

## Wire format

Plugins are C-ABI shared libraries (`-buildmode=c-shared`) exporting
`cliproxy_plugin_init`, which installs a `{call, free_buffer, shutdown}` vtable.
Every call is a JSON envelope `{ok, result, error}` over that boundary. There is
no Go toolchain or dependency lockstep with the host.

Casing is mixed and load-bearing: host-bound callback requests use snake_case,
while payloads modelled on the host's `sdk/pluginapi` structs are untagged Go
structs there and so travel as **PascalCase**. `internal/runtime/wire.go` encodes
which is which. Do not "normalise" the tags.

## Architecture

```
main.go              cgo boundary, panic guard, method dispatch
internal/model       domain types + config; imports nothing else here
internal/quota       usage-endpoint client, response-header parsing, snapshots
internal/pace        the scoring curve; pure functions over model types
internal/session     conversation identity extraction + binding store
internal/runtime     wire types, hook handlers, plugin lifecycle, decision log
internal/web         embedded status app served on the plugin's routes
```

`internal/model` is the only package the others share. `pace` and `session` are
pure and fully unit-testable without the host.

## Rules for changes

- No blocking I/O on the pick path. Ever.
- Every exported cgo entry point recovers from panics.
- Prefer declining (`Handled: false`) over guessing.
- Tests must not reach the network. The usage client takes a base URL so tests
  point it at a stub.
- Never log a token, a refresh token, or a request body.
