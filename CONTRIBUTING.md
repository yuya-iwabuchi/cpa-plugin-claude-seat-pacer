# Contributing

One person maintains this plugin, with no support commitment. Bug reports and
questions go to
[Issues](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/issues);
the README's Troubleshooting section covers the failures seen so far, so check
it first.

## Changes

Open an issue before writing code. The plugin runs inside a proxy that holds
Claude OAuth tokens, so every change is reviewed as credential-handling code,
and whether a change is worth that review is settled in the issue first. A
pull request with no issue behind it is closed. When an issue settles on a
change you want to make, say so there and a pull request is arranged.

The MIT licence permits a fork. When your needs diverge from this plugin's
design, a fork is the intended path rather than a pull request that moves it.

Report a vulnerability privately through the repository's Security tab rather
than as an issue.

## Build and test

`make build` writes the shared library under `dist/`, and `make install` copies
it into the host's plugin directory. Both need Go 1.25 or newer and a C toolchain,
because `-buildmode=c-shared` needs cgo. `go.mod` pins the patch release in its
`toolchain` directive, and the `go` command downloads it when yours is older.
A darwin/amd64 build also compiles that Go release from source with
`.github/scripts/go-tls-slot.patch` applied, a few minutes the first time;
AGENTS.md says why.

`make test` runs the tests under the race detector. Tests must not reach the
network: the usage client sends every request through an injected Doer, which
tests stub. The two tests that drive a real CLIProxyAPI process skip unless
`CPA_SOURCE_DIR` names a host checkout; `internal/runtime/WIRE.md` carries the
invocation. The test that parses the status page's script skips where `node` is
not installed.

`go run ./cmd/webdev` serves the status page on `127.0.0.1:8377` from fixture
seats, with `-scenario` choosing the pool it shows, so a page change can be
seen without a host. `-scenario replay -history <file>` draws the seats of a
recorded history file instead, `-at` renders it as of a recorded instant, and
`-locks` adds refusal spans to the ones the file records. Replaying an install's
own `history.json` names each seat by its credential file's note, read from the
directory two above the history file's own, and publishes ids keyed with a copy
of the `page-id.key` beside it, as the host page does. A locks file is a JSON
object keyed by credential id, each holding a list of spans
`{"kind", "scope", "from", "to", "cut_by_success"}` with `from` and `to` in Unix
seconds; `cut_by_success` marks a span a served request ended, and any other
ran to its reset.

## Landing an arranged change

`make check` is the gate: `gofmt`, `go vet`, the tests under the race detector
and the `c-shared` build. CI runs it on every pull request.

Commit subjects follow `Type(N/A): what is true once the change lands`, where
Type is `Fix`, `Feature`, `Chore`, `Refactor`, `Docs` or `Maintenance`. The
body states the fact the code embodies, in present tense, rather than
narrating the change; `git log` shows the register.

Read [AGENTS.md](AGENTS.md) before touching the pick path, the cgo boundary or
the wire types: it holds the architecture, the host invariants a change must
not break, and the rules for changes.
