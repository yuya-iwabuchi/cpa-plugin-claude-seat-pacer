# Claude Seat Pacer

Spreads Claude requests across your seats by burn pace, and pins each conversation so its cache keeps hitting.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/hero-dark.png">
  <source media="(prefers-color-scheme: light)" srcset="docs/hero-light.png">
  <img src="docs/hero-light.png" width="900" alt="Screenshot of the Claude Seat Pacer status page, showing the live bar naming the seat the next new conversation will land on, three seats ranked by pace score with the winner highlighted, and the weekly pace plot with the cost-ordered seat list beside it.">
</picture>

## Why

Prompt caching carries a Claude Code session: on real traffic about 96% of
input tokens are cache reads, which cuts input cost roughly 7x. Caches never
cross an Anthropic organization, so a router that moves a live conversation to
another seat pays a full-price miss on that request. Weekly quota is
perishable: budget left on a seat when its window resets is gone. This
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin pins each
conversation to one seat and starts new conversations on the seat whose budget
expires soonest.

## Install

The plugin builds from source. There is no prebuilt release and no Plugin
Store listing yet; both are the next milestone.

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

### Requirements

Supported host: CLIProxyAPI 7.2.145 or newer, verified against 7.2.149. The
build needs Go 1.25 and a C toolchain.

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
affinity cache, so both cannot hold it. Give every Claude seat in the pool one
`priority` value: the host offers the plugin only the highest tier, so a seat
alone at the top takes every new conversation and the seats beneath it are a
fallback the host uses on its own. That layout works, but the plugin has
nothing to spread across; the status page names the seats when it sees it.

A seat is named by its credential's **note**, set on the auth-file card in the
Management Center or as a `note` key in the credential file. Without one the
host names it by its account email, which the status page masks to
`y…@example.com`.

### All settings

The dials people touch are `landing-target`, `affinity.ttl`,
`override-threshold` and `poll-interval`; the rest hold up unattended.
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
        poll-interval: 2m       # usage-endpoint read cadence; below 30s falls back to the default
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

## Status page

The page shows every seat's quota windows, utilization history, pace score and
eligibility for a chosen model; the live conversation bindings per seat; the
recent routing decisions with the scores behind each; and warnings for whatever
leaves the plugin inert or degraded (disabled by config, a failing credential
listing, a single-candidate pool, a seat whose usage read fails).

The Management Center gains a "Claude Seat Pacer" entry that opens it. The page
is also served at `/v0/resource/plugins/claude-seat-pacer/index.html`, with
the JSON behind it at `.../api/status?model=<id>` on the same unauthenticated
prefix. Authenticated routes under `/v0/management/plugins/claude-seat-pacer/`
are `GET status`, `POST refresh`, `POST unbind?auth_id=`, and
`POST bindings/sweep`.

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
default `override-threshold` it stays even when the seat trails the curve or is
spent, because the cache hit is worth more than the rebalance. When no seat is
eligible a conversation still gets one stable home, the seat with the fewest
live conversations; a request with neither a session id nor a usable reading
is declined to the host's own selector.

## Privacy and data

The plugin calls one external endpoint, `usage-url`, and reads it with each
seat's own OAuth access token, as the host already holds it; the read reports
per-window utilization.

The status page and its JSON are served without the management key, so seat
ids are hashed and emails masked before they ship, and operator warnings drop
filesystem paths.

`~/.cli-proxy-api/plugins/claude-seat-pacer/history.json` holds per-seat
utilization samples so the page's charts survive a host restart. It carries
credential ids and utilization fractions, nothing else and no token.

No token, refresh token, or request body is ever logged; an error message that
would carry an access token is scrubbed before it is recorded.

## Troubleshooting

**The host does not list the plugin.** The host derives the plugin id from the
library filename, minus the extension and the `-v<version>` suffix, and the
config block, the management routes and the history directory all carry that
id. Keep the installed filename as `make install` writes it, and restart the
host after installing.

**The plugin loads but every new conversation lands on one seat.** Either
`enabled` is false, or that seat sits alone on the highest `priority` tier and
the rest are its fallback. The status page warns in both cases and names the
seats; set `enabled: true`, or give every seat in the pool the same `priority`
so the plugin can spread by pace.

**Conversations do not stick to a seat.** The host's `routing.session-affinity`
is still on. The plugin owns affinity and a plugin pick never seeds the host's
cache, so both cannot hold it; set it to `false`. Nothing the plugin receives
distinguishes the two states, so the status page does not warn about this one.

**Seats show as ineligible.** Their readings are missing or older than
`max-staleness`, or their usage read is failing. The status page names the
reason per seat and carries the poll error; wait one `poll-interval`, or
`POST refresh` to read now.

**The config block seems ignored.** A block that does not parse loads the
plugin with defaults and `enabled: false`, and a key holding an out-of-range
value falls back to its default. The status page warns that the plugin is
disabled; fix the YAML and restart the host.

## Contributing

Questions and reports go to [Issues](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/issues).
Pull requests are welcome when they pass `make check`. [CONTRIBUTING.md](CONTRIBUTING.md)
covers the commit convention and points at the architecture notes.

## Licence

MIT (SPDX: `MIT`); see `LICENSE`. `NOTICE` carries the attribution for the
CLIProxyAPI ABI types this plugin mirrors.
