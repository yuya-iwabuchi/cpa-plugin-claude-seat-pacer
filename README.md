# Claude Seat Pacer

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that
spreads Claude requests across your OAuth subscription seats by how fast each
one is spending its quota, and keeps every conversation on the seat it started
on so its prompt cache keeps hitting.

## Why

Prompt caching carries a Claude Code session: on real traffic about 96% of
input tokens are cache reads, which cuts input cost roughly 7x. Caches never
cross an Anthropic organization, so a router that moves a live conversation to
another seat pays a full-price miss on that request. Weekly quota is
perishable: budget left on a seat when its window resets is gone. Round-robin
ignores the first fact and fallback routing ignores the second; this plugin
pins each conversation to one seat and starts new conversations on the seat
whose budget expires soonest.

## Install

Supported host: CLIProxyAPI 7.2.145 or newer, verified against 7.2.149. The
build needs Go 1.25 and a C toolchain.

```sh
git clone https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer
cd cpa-plugin-claude-seat-pacer
make install
```

`make install` builds the shared library into
`~/.cli-proxy-api/plugins/<goos>/<goarch>/claude-seat-pacer-v0.1.0.<dylib|so|dll>`
(`PLUGIN_DIR` overrides the root). The host loads plugins once at startup, so
restart it: `brew services restart cliproxyapi` on Homebrew, or however your
CLIProxyAPI runs.

## Configure

Add the block under `plugins.configs` and turn the host's own affinity off:

```yaml
routing:
  session-affinity: false

plugins:
  configs:
    claude-seat-pacer:
      enabled: true
```

The plugin owns conversation affinity, and a plugin pick never seeds the host's
affinity cache, so both cannot hold it. Every Claude seat in the pool must share
one `priority` value: the host offers the plugin only the highest tier, and a
single-seat tier leaves nothing to spread across. The status page warns when
that happens.

Defaults, all optional:

```yaml
      providers: [claude]       # provider keys the plugin routes; others fall to the host
      models: []                # model ids the plugin routes; empty means all
      affinity:
        enabled: true
        ttl: 1h                 # idle time before a conversation's binding expires
        subagents: true         # a subagent shares its parent conversation's seat
        override-threshold: true # keep a binding even when its seat trails the pace curve
        max-sessions: 65536     # bindings held before the oldest idle one is dropped
      pace:
        shape: linear           # linear, power or sigmoid
        curve-exponent: 1.0     # power shape only; above 1 holds back early
        steepness: 8.0          # sigmoid shape only
        landing-target: 1.10    # utilization the curve aims for at window end; 0 < x <= 4
        weekly-weight: 1.0      # weight of the all-models weekly window
        scoped-weight: 0.5      # weight of a model-family weekly window
        session-weight: 0       # weight of the 5-hour window; it is a rate limit, not a budget
        hysteresis-margin: 0.05 # cost gap a challenger must beat to move a binding
      quota:
        poll-interval: 2m       # usage-endpoint read cadence; minimum 30s
        request-timeout: 10s    # one usage read
        max-staleness: 15m      # a reading older than this makes the seat ineligible
        persist-history: true   # keep utilization history across host restarts
        usage-url: https://api.anthropic.com/api/oauth/usage
      web:
        enabled: true           # serve the status page
        history-limit: 500      # routing decisions kept for the page
```

Durations take Go syntax (`30s`, `2m`, `1h`). A value out of range falls back
to its default; a block that does not parse loads the plugin disabled.

## What you see

The Management Center gains a "Claude Seat Pacer" entry that opens the status
page, also served at `/v0/resource/plugins/claude-seat-pacer/index.html`; the
JSON behind it is at `.../api/status?model=<id>` on the same unauthenticated
prefix. It
shows every seat's quota windows, utilization history, pace score and
eligibility for a chosen model; the live conversation bindings per seat; the
recent routing decisions with the scores behind each; and warnings for whatever
leaves the plugin inert or degraded (disabled by config, a failing credential
listing, a single-candidate pool, a seat whose usage read fails). The page is
unauthenticated, so seat ids are hashed and emails masked. Authenticated routes
under `/v0/management/plugins/claude-seat-pacer/` are `GET status`,
`POST refresh`, `POST unbind?auth_id=`, and `POST bindings/sweep`.

## How the pick works

For a new conversation the plugin scores every seat the host offers: each
window's slack is the pace target at this point in the window minus the
observed utilization, and the seat's cost is the negated weighted sum of those
slacks, so the seat furthest behind its curve wins. The target is
`landing-target` times the curve shape, clamped to full; at the default 1.10 a
seat near its reset is expected to have spent more, so it is favoured while its
budget can still be used. A seat is ineligible when Anthropic has rejected a
request on one of its windows, a window reads as full, a reading is not a
number, none of its windows covers the requested model family, its usage read
is failing, or its snapshot is missing or older than `max-staleness`. A
conversation stays on its seat while the host still offers it; a provider
refusal for the model moves it only when another seat can take that model,
because a move with nowhere better to go is a cache miss for nothing. With the
default `override-threshold` it
stays even when the seat trails the curve or is spent, because the cache hit is
worth more than the rebalance. When no seat is eligible a conversation still
gets one stable home, the seat with the fewest live conversations; a request
with neither a session id nor a usable reading is declined to the host's own
selector.

## Files

`~/.cli-proxy-api/plugins/claude-seat-pacer/history.json` holds per-seat
utilization samples so the page's charts survive a host restart. It carries
credential ids and utilization fractions, nothing else and no token.

## Licence

MIT; see `LICENSE`. `NOTICE` carries the attribution for the CLIProxyAPI ABI
types this plugin mirrors.
