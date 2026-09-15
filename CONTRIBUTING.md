# Contributing

Questions, bug reports and feature requests go to
[Issues](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/issues).

Pull requests are welcome. `make check` is the gate: it runs `gofmt`, `go vet`,
the tests under the race detector, and the `c-shared` build of the library.
Tests must not reach the network.

Commit subjects follow `Type(N/A): what is true once the change lands`. The
body states the fact the code embodies, in present tense, rather than
narrating the change; `git log` shows the register.

[AGENTS.md](AGENTS.md) holds the architecture, the host invariants a change
must not break, and the rules for changes. Read it before touching the pick
path, the cgo boundary or the wire types.
