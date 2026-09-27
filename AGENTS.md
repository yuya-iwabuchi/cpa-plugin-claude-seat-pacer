# Agent brief

Host line references are to CLIProxyAPI v7.2.149 source (tag `v7.2.149`, commit
`2a6b87ac`). The host-driven tests pass against v7.3.10 and v7.3.15. The
deployment target is 7.2.145+, a lower bound nothing in this repository tests.

## What this plugin is

A CLIProxyAPI plugin that uses up each Claude OAuth subscription seat's weekly
quota before it resets. Every new conversation goes to the seat furthest behind
a plan that spends its quota steadily and finishes a little before the reset,
and each conversation stays on one seat so Anthropic prompt caches keep
hitting.

The preference is a policy, not a bug to fix: `LandingTarget` defaults above
full on purpose, because weekly quota left at reset is lost. 1.0 removes only
the extra lean that plans each seat to finish early; a seat still holding
quota near its reset is behind its plan either way and still goes first. An
even spread is the host's round-robin, with the plugin disabled. Changing that
default changes what the plugin is for.

## The two facts the whole design rests on

**1. A moved conversation loses its prompt cache.** In a long conversation
nearly every input token is a cache read, and a cache read costs a small
fraction of a fresh input token. Anthropic isolates caches between
organizations, so moving a live conversation to a credential in another org
pays full price, plus the cache write, for everything it has already sent.

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
  plugin, so a blocking call there parks a goroutine permanently and blocks
  `dlclose`. The pick reads in-memory state only; all fetching happens on a
  background goroutine.
- **An escaped panic terminates the proxy.** `recover()` does not cross the C
  boundary, and the host's guard runs in its own Go runtime. Every exported
  entry point in `main.go` recovers, `runtime.Plugin.Call` recovers again, and
  every goroutine the plugin starts runs its work under a recover: a host
  callback's panic becomes that call's error, the poll loop runs through
  `guard`, the usage fetch turns one into a failed fetch, and `host.spawn`
  drops whatever else escapes.
- **Never return an error envelope from `scheduler.pick`.** It hard-fails the
  request with no fallback to the host's selector. `Handled: false` is the safe
  decline and the answer to anything the plugin is unsure about;
  `runtime.Degrade` gives each method its least damaging failure.
- **`pick` is called concurrently with no serialization.** Config is held in an
  `atomic.Pointer`; the quota store, binding store and decision log lock
  internally; everything else mutable sits behind a mutex held only for field
  access and never across a host callback.
- **Candidates are pre-filtered and capped at the highest priority tier.**
  Disabled, cooling, model-incompatible and already-tried credentials are gone
  before the plugin is asked, and lower tiers are never offered, so every seat
  must share one `priority`. The status page warns while the pick sees a single
  candidate.
- **A plugin pick never seeds the host's affinity cache**: `SessionCache.Touch`
  refuses to create entries, so a hybrid where the host keeps affinity and the
  plugin only biases cold starts cannot bootstrap. A plugin that answers
  `Handled: true` also skips the host's selector, `SessionAffinitySelector`
  included. This plugin owns affinity; run with
  `routing.session-affinity: false`. `host.affinity.lookup` (host 7.2.156+)
  answers `unsupported` while a plugin scheduler is wired, so it tells this
  plugin nothing.
- **Only one scheduler plugin is ever consulted**: the first non-fused one by
  plugin priority, then id.
- **Home mode bypasses the scheduler hook entirely**, while
  `request.intercept_before` still runs. No field the plugin receives names the
  mode, so the status page cannot warn about it; the symptom is a decision log
  that stays empty under live traffic.
- **Declare `schema_version: 1`**: the host refuses a plugin whose declared
  version exceeds its own.
- **Resource routes are served to anyone who can reach the host's port**, with
  no key and no loopback check, so the resource route carries the static page
  and nothing else. The page's data is the `page-status` management route,
  which needs the key and, unless `remote-management.allow-remote` is set, a
  loopback client. For a plugin declaring a schema below 6 the host
  HTML-escapes every string in a management JSON body, and the page restores
  the text once; raising it to 6 or above ends the escaping, and the page's
  decode goes with it. The host bans a client address, loopback included, for
  30 minutes after five failed key checks, a request with no key among them,
  so the page never sends a request without a key and stops at the first
  refusal.

## Wire format

Plugins are C-ABI shared libraries (`-buildmode=c-shared`) exporting
`cliproxy_plugin_init`, which installs a `{call, free_buffer, shutdown}` vtable.
Every call is a JSON envelope `{ok, result, error}` over that boundary.

Casing is mixed and load-bearing: host-bound callback requests use snake_case,
while payloads modelled on the host's `sdk/pluginapi` structs are untagged Go
structs there and so travel as **PascalCase**. `internal/runtime/wire.go` encodes
which is which. Do not "normalise" the tags.

## Architecture

```
main.go              cgo boundary, panic guard, method dispatch
internal/model       domain types + config; imports nothing else here
internal/httpx       case-insensitive header index; imports nothing else here
internal/quota       usage-endpoint client, response-header parsing, snapshots, utilization history and refusal spans
internal/pace        the scoring curve and the staleness gates; pure functions over model types
internal/session     conversation identity extraction + binding store
internal/runtime     wire types, hook handlers, plugin lifecycle, decision log, status and warnings
internal/web         embedded status app: page on the resource route, data behind the key
cmd/webdev           fixture server that renders the status app from synthetic seats or a recorded history file
```

`model` and `httpx` are the leaves: every other package imports `model`, and
`quota`, `session` and `runtime` import `httpx`. `pace` and `session` are pure
and unit-testable without the host.

## Rules for changes

- Tests must not reach the network. The usage client sends every request
  through an injected Doer, which tests stub.
- Never log a token, a refresh token, or a request body.
- The usage URL receives every seat's token, so `model.Config.Normalize`
  admits only `https://api.anthropic.com` and loopback `http`.
- The page publishes credential ids and conversation keys as an HMAC under
  the install key in `page-id.key`, or a random per-process key when that file
  cannot be used. Never fall back to an unkeyed hash: a credential id is
  routinely an account address and a conversation key can digest a prompt, and
  an unkeyed digest lets a screenshot confirm a guess. Emails are masked to
  their first letter and domain, and an operator's `note` appears as written.
- A comment states a present-tense fact the code cannot show. Change history
  and rationale go in the commit or PR.
- Config bounds live in `model.Config.Normalize`: give a new setting its range
  there, not in the code that reads it. A setting clamped to its upper bound,
  or one the operator set that is raised to fit another, returns a warning,
  which the status page shows.
- The Management Center saves each `configFields` entry as a top-level key
  under its dotted name, which `decodeConfig` expands, winning over the nested
  form. A field's `Name` is therefore the dotted YAML path of the key it sets.
  `enabled` has no field: the host renders its own toggle.
- `pluginName` in `main.go`, `NAME` in the Makefile, the history path in
  `internal/runtime/poller.go`, the library name in `.github/workflows/` and
  `.github/scripts/`, and the plugin store registry `id` stay equal: the host
  derives the plugin id from the library filename, and the config block, the
  management routes and the history directory all carry that id.
- `internal/web/index.html` holds no NUL or CR byte: the CSP hashes the inline
  script from the file's bytes, and the HTML tokenizer rewrites both
  (`TestPageHoldsNoRewrittenByte`).
- `go.mod` pins the Go toolchain with a `toolchain` directive, and govulncheck
  (`vuln.yml`) judges that toolchain's standard library: a standard-library
  finding is fixed by raising the pin, a dependency finding by raising that
  module's requirement.
- Both workflows set `MACOSX_DEPLOYMENT_TARGET` to the oldest macOS the pinned
  Go supports (12.0 for Go 1.25 and 1.26), and the release fails a macOS
  library recording another minimum. A toolchain that drops a macOS release
  raises the target and the README's stated minimums with it.
- The darwin/amd64 library builds with Go compiled from upstream source plus
  `.github/scripts/go-tls-slot.patch`, which moves the runtime's goroutine
  pointer from TSD slot 6 (`%gs:0x30`) to slot 11 (`%gs:0x58`). Stock Go puts
  every runtime in slot 6, so a stock-built plugin reads CLIProxyAPI's
  goroutine on host threads and corrupts the host's heap; the other platforms
  give each runtime its own slot and build with stock Go. Every library built
  with this patch shares slot 11, so an Intel host holds at most one of them
  and no stock-built Go plugin. `build-darwin-amd64.sh` refuses a `toolchain`
  pin other than its own `GO_VERSION`, so raising the pin means setting that
  version and its source checksum and confirming the patch still applies. The build fails on any
  `%gs:0x30` access, and the release publishes the library only after
  `load-test-darwin-amd64.sh` loads it into the official CLIProxyAPI on
  `macos-15-intel`.

## Releasing

Every release keeps the plugin store's install contract. It ships
`checksums.txt` and one `claude-seat-pacer_<version>_<goos>_<goarch>.zip` for
each of darwin amd64/arm64, linux amd64/arm64 and windows amd64, each holding
the library at its root, and `release.yml`'s assemble step fails on any other
set. The resource route serves the static page alone
(`TestManagementRegisterDeclaresRoutesAndResources`,
`TestManagementAdapterStripsResourcePrefix`): new data or actions go on a
management route behind the key.

1. Set `pluginVersion` in `main.go` to the new version and merge it to `main`.
2. On an up-to-date, clean `main`, run `make release-tag`. It refuses another
   branch, a dirty tree, a `main` that differs from `origin/main`, or a
   version already tagged, then pushes `v<version>`.
3. `release.yml` builds and checks every platform and publishes the release.
   Releases are immutable and their tags cannot move, so a mistake ships as the
   next version.
4. Verify the published zips with the commands in `SECURITY.md`, and run the
   plugin registry's Validate workflow (`workflow_dispatch`) to confirm the
   release installs on every platform. Plugin Store installs offer the update
   within about an hour; the operator applies it with Update, and the new file
   loads without a host restart.
