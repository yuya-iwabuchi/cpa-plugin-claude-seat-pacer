# Agent brief

Verified against CLIProxyAPI v7.2.149 source (tag `v7.2.149`, commit `2a6b87ac`); the
deployment target is 7.2.145+, a lower bound nothing in this repository tests.

## What this plugin is

A CLIProxyAPI plugin that spends the Claude OAuth subscription seat whose
weekly budget expires soonest first, and keeps each conversation on one seat
so Anthropic prompt caches keep hitting.

The preference is a policy, not a bug to fix: `LandingTarget` defaults above
full on purpose, because weekly quota left at reset is lost, and a pool that
wants an even spread sets it to 1.0. Changing that default changes what the
plugin is for.

Declared capabilities: `request_interceptor`, `scheduler`, `usage_plugin`,
`management_api`.

## The two facts the whole design rests on

**1. Prompt caching is worth ~7x, and caches never cross an organization.**
Measured over 2,096 real requests: 96.5% of input-side tokens are cache reads,
and caching removes 86% of input-side cost. Anthropic documents that caches are
isolated between organizations, so moving a live conversation to a credential in
another org is a guaranteed full miss costing ~12.5x on that request.

**2. `request.intercept_before` is the only way to learn the conversation id.**
`scheduler.pick` receives `Headers` and `Metadata` but **not the request body**,
and for Claude Code the session id lives in the body (`metadata.user_id`), not a
header. `request.intercept_before` does receive the full body, and header
mutations it returns survive into `opts.Headers`, which the host clones verbatim
into `SchedulerOptions.Headers`. So the interceptor derives the key and injects
a private header; the scheduler reads it back.

This header bridge is emergent behaviour, not a documented contract.
`TestHeaderBridgeSurvivesToSchedulerPick` asserts it round-trips through a real
host; it skips without `CPA_SOURCE_DIR`, and `internal/runtime/WIRE.md` carries
the run. Do not remove that test.

## Host invariants that constrain the code

- **`scheduler.pick` has no timeout.** Context cancellation does not unwind the
  plugin. Any blocking call there parks a goroutine permanently and blocks
  `dlclose`. The pick reads an in-memory snapshot only; all fetching happens on
  a background goroutine.
- **An escaped panic terminates the proxy.** `recover()` does not cross the C
  boundary: the host's guard, and the fuse it trips to disable every capability
  a plugin declares until the library is replaced, run in the host's own Go
  runtime and never see a panic raised inside this one. Every exported entry
  point in `main.go` installs its own recover, and `runtime.Plugin.Call` a
  second, so a failure degrades to a decline.
- **`pick` is called concurrently with no serialization.** Config is held in an
  `atomic.Pointer`; the quota store, binding store and decision log lock
  internally; everything else mutable sits behind a mutex held only for field
  access and never across a host callback.
- **Never return an error envelope from `scheduler.pick`.** It hard-fails the
  request with no fallback to the host's selector. `Handled: false` is the safe
  decline, and it is the default for anything the plugin is unsure about.
- **Candidates are pre-filtered and capped at the highest priority tier.**
  Disabled, cooling, model-incompatible and already-tried credentials are gone
  before the plugin is asked, and lower priority tiers are never offered. All
  credentials in the pool must share one `priority` value or spreading silently
  does nothing. The pick records a single-candidate list; the poller logs it
  once and the status page warns while it holds.
- **A plugin pick never seeds the host's affinity cache.** `SessionCache.Touch`
  refuses to create entries, so a hybrid where the host keeps affinity and the
  plugin only biases cold starts cannot bootstrap. This plugin owns affinity;
  run with `routing.session-affinity: false`.
- **`host.affinity.lookup` is a read-only observation and changes none of the
  above.** The callback (host 7.2.156+, commit `0796d6d1`) takes provider,
  model and session id and answers `bound`, `unbound`, `ambiguous` or
  `unsupported`, plus the bound credential's auth index. A plugin that answers
  `Handled: true` from `scheduler.pick` short-circuits the host's built-in
  selector, so `SessionAffinitySelector` never runs and never creates a
  binding: where this plugin owns the pick, no conversation it routes is ever
  `bound`, and `Manager.LookupSessionAffinity` answers `unsupported` outright
  while a plugin scheduler is wired. The callback exists for a plugin that does
  not own `scheduler.pick` — one that only rewrites credential priority, say.
- **Only one scheduler plugin is ever consulted**: the first non-fused one by
  plugin priority, then id. Mutually exclusive with other scheduler plugins.
- **Home mode bypasses the scheduler hook entirely.** The host dispatches
  through Home before it consults any plugin scheduler, while
  `request.intercept_before` still runs. No field the plugin receives names
  the mode, so the status page cannot warn about it; the symptom is a decision
  log that stays empty under live traffic.
- **Declare `schema_version: 1`**, not the host's current value: the host
  refuses a plugin whose declared version exceeds its own, so a higher value
  makes every older host refuse the load.

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
internal/httpx       case-insensitive header index; imports nothing else here
internal/quota       usage-endpoint client, response-header parsing, snapshots, utilization history
internal/pace        the scoring curve and the staleness gates; pure functions over model types
internal/session     conversation identity extraction + binding store
internal/runtime     wire types, hook handlers, plugin lifecycle, decision log, status and warnings
internal/web         embedded status app served on the plugin's resource routes
cmd/webdev           fixture server that renders the status app from synthetic seats
```

`model` and `httpx` are the leaves: every other package imports `model`, and
`quota`, `session` and `runtime` import `httpx`. `pace` and `session` are pure
and fully unit-testable without the host.

## Rules for changes

- No blocking I/O on the pick path. Ever.
- Every exported cgo entry point recovers from panics.
- Prefer declining (`Handled: false`) over guessing.
- Tests must not reach the network. The usage client does no networking of
  its own: every request goes through an injected Doer, which tests stub.
- Never log a token, a refresh token, or a request body.
- A comment states a present-tense fact the code cannot show. Change history
  and rationale go in the commit or PR.
- `internal/web/index.html` holds no NUL or CR byte: the CSP hashes the inline
  script from the file's bytes, and the HTML tokenizer rewrites both, so either
  byte ships a hash no browser matches (`TestPageHoldsNoRewrittenByte`).
- `pluginName` in `main.go`, `NAME` in the Makefile, the history path in
  `internal/runtime/poller.go` and the library name in `.github/workflows/`
  stay equal: the host derives the plugin id from the library filename, and
  the config block, the management routes and the history directory all carry
  that id.
