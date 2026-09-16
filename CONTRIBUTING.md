# Contributing

Questions, bug reports and feature requests go to
[Issues](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/issues).

## Build and test

`make build` writes the shared library under `dist/`, and `make install` copies
it into the host's plugin directory. Both need Go 1.25 and a C toolchain,
because `-buildmode=c-shared` needs cgo.

`make test` runs the tests under the race detector. Tests must not reach the
network: the usage client sends every request through an injected Doer, which
tests stub. The two tests that drive a real CLIProxyAPI process skip unless
`CPA_SOURCE_DIR` names a host checkout; `internal/runtime/WIRE.md` carries the
invocation.

`go run ./cmd/webdev` serves the status page on `127.0.0.1:8377` from fixture
seats, with `-scenario` choosing the pool it shows, so a page change can be
seen without a host.

## Pull requests

`make check` is the gate: `gofmt`, `go vet`, the tests under the race detector
and the `c-shared` build. CI runs it on every pull request.

Commit subjects follow `Type(N/A): what is true once the change lands`, where
Type is `Fix`, `Feature`, `Chore`, `Refactor`, `Docs` or `Maintenance`. The
body states the fact the code embodies, in present tense, rather than
narrating the change; `git log` shows the register.

[AGENTS.md](AGENTS.md) holds the architecture, the host invariants a change
must not break, and the rules for changes. Read it before touching the pick
path, the cgo boundary or the wire types.
