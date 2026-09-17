# Claude Seat Pacer

Spends the Claude seat that resets soonest first, and keeps each conversation on one seat so its prompt cache holds.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/hero-dark.png">
  <source media="(prefers-color-scheme: light)" srcset="docs/hero-light.png">
  <img src="docs/hero-light.png" width="900" alt="Screenshot of the Claude Seat Pacer status page, showing the live bar naming the seat the next new conversation will land on, three seats ranked by pace score with the winner highlighted, and the weekly pace plot with the cost-ordered seat list beside it.">
</picture>

## Why

Weekly quota is perishable: whatever a seat has not spent when its window
resets is gone. With one seat resetting tomorrow and another reset this
morning, the seat to drain is tomorrow's, and the fresh one can wait a week.
CLIProxyAPI's own routing spreads evenly or fills the first seat, and either
way budget expires unspent.

Two facts shape how this [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
plugin does it. A prompt cache never crosses an Anthropic organization, and on
real Claude Code traffic about 96% of input tokens are cache reads, so moving
a live conversation to another seat pays full price for that request; a
conversation therefore stays where it started. And the 5-hour window is a
rate limit rather than a budget — it resets several times a day and carries
nothing over — so it can veto a seat but does not rank one; the weekly
windows decide where a new conversation goes.

I built this for my own pool, and the defaults encode my preference: a new
conversation starts on the seat furthest behind a spend curve that lands past
full, which is the seat whose budget expires soonest. A pool where even spread
matters more than draining the closing window wants `landing-target: 1.0`,
and a design that has to diverge further is a fork, not a pull request.

## Install

Build from source; there is no prebuilt release or Plugin Store listing yet.
The build needs Go 1.25 and a C toolchain, and the host must be CLIProxyAPI
7.2.145 or newer (verified against 7.2.149).

```sh
git clone https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer
cd cpa-plugin-claude-seat-pacer
make install
```

This writes `~/.cli-proxy-api/plugins/<goos>/<goarch>/claude-seat-pacer-v0.1.0.<dylib|so|dll>`
(`PLUGIN_DIR` overrides the root). The host loads plugins once at startup, so
restart it — `brew services restart cliproxyapi` on Homebrew.

## Configure

Turn the host's own affinity off and enable the plugin:

```yaml
routing:
  session-affinity: false

plugins:
  configs:
    claude-seat-pacer:
      enabled: true
```

The plugin owns conversation affinity, and a plugin's pick never seeds the
host's cache, so the two cannot share it. Give every seat in the pool the same
`priority`: the host offers the plugin only the top tier, and a seat alone
there takes every new conversation with the rest as the host's own fallback.
The status page warns when it sees that.

A seat is named by its credential's `note` (the auth-file card in the
Management Center, or a `note` key in the file); without one, by its account
email, masked to `y…@example.com`.

### All settings

Every key is optional. `landing-target`, `affinity.ttl`,
`override-threshold` and `poll-interval` are the ones operators change; the
rest hold up unattended.

```yaml
      providers: [claude]       # provider keys the plugin routes; others fall to the host
      models: []                # model ids the plugin routes; empty means all
      affinity:
        enabled: true
        ttl: 1h                 # idle time before a conversation's binding expires
        subagents: true         # a subagent shares its parent conversation's seat
        override-threshold: true # keep a binding even when its seat trails the pace curve
        max-sessions: 65536     # bindings held before the least recently seen is dropped
      pace:
        shape: linear           # linear, power or sigmoid
        curve-exponent: 1.0     # power shape only; above 1 holds back early
        steepness: 8.0          # sigmoid shape only
        landing-target: 1.10    # utilization the curve aims for at window end; 0 < x <= 4
        weekly-weight: 1.0      # weight of the all-models weekly window
        scoped-weight: 0.5      # weight of a model-family weekly window
        session-weight: 0       # weight of the 5-hour window; it is a rate limit, not a budget
        hysteresis-margin: 0.05 # cost gap a challenger must beat to move a binding when override-threshold is off
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

Durations take Go syntax (`30s`, `2m`, `1h`). An out-of-range value falls back
to its default; a block that does not parse loads the plugin disabled.

## How it picks

A new conversation goes to the seat furthest behind its curve. Each weekly
window's slack is the curve's target at this point in the window minus the
seat's observed utilization, and the target is `landing-target` times the
curve shape, clamped to full — so at the default 1.10 a seat near its reset is
expected to have spent more, and wins while its budget can still be used. The
seat's cost is the negated weighted sum of its slacks.

A seat is ineligible when Anthropic has refused a request on one of its
windows, a window reads full or not as a number, no window covers the
requested model family, its usage read has never succeeded, or its reading is
older than `max-staleness`.

A conversation stays on its seat while the host still offers it, and with the
default `override-threshold` stays even when the seat trails the curve or is
spent, because the cache hit is worth more than the rebalance. A refusal for
the model moves it only when another seat can take that model. When no seat is
eligible a conversation still gets one stable home, the seat with the fewest
live conversations; a request with neither a session id nor a usable reading
is declined to the host's own selector.

## Status page

The Management Center gains a "Claude Seat Pacer" entry. The page shows each
seat's windows, utilization history, pace score and eligibility for a chosen
model; the live bindings per seat; recent decisions with their scores; and a
warning for whatever leaves the plugin inert or degraded.

It is served at `/v0/resource/plugins/claude-seat-pacer/index.html`, with its
JSON at `.../api/status?model=<id>`, both without the management key. The
authenticated routes under `/v0/management/plugins/claude-seat-pacer/` are
`GET status`, `POST refresh`, `POST unbind?auth_id=` and `POST bindings/sweep`.

## Privacy and data

The plugin calls one external endpoint, `usage-url`, through the host's HTTP
client with each seat's OAuth token, which the host already holds; the read
reports per-window utilization.

The status page is served without the management key, so seat ids are hashed,
emails masked, and URLs and paths dropped from warnings before they ship. A
credential's `note` is a seat's name and is published as written, so it is
readable by anyone who can reach the page.

`~/.cli-proxy-api/plugins/claude-seat-pacer/history.json` keeps per-seat
utilization samples across restarts: credential ids (file names or account
emails) and timestamped fractions, no token. No token, refresh token or
request body is ever logged, and an error that would carry a token is scrubbed
first.

## Troubleshooting

**The host does not list the plugin.** The plugin id comes from the library
filename minus its extension and `-v<version>`, and the config block, routes
and history directory all carry it. Keep the filename `make install` writes,
and restart the host.

**Every new conversation lands on one seat.** `enabled` is false, or that seat
is alone on the top `priority` tier. The status page warns and names the seats
in both cases.

**Conversations do not stick.** The host's `routing.session-affinity` is still
on; set it to `false`. Nothing the plugin receives distinguishes the two
states, so the page cannot warn about this one.

**Seats show as ineligible.** Their reading is missing, older than
`max-staleness`, or has never succeeded. The page names the reason per seat
and carries the poll error; wait one `poll-interval` or `POST refresh`.

**The config block seems ignored.** A block that does not parse loads the
plugin disabled with defaults, and an out-of-range value falls back to its
default. The page warns that the plugin is disabled; fix the YAML and restart.

## Contributing

Bug reports and questions go to [Issues](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/issues).
Open an issue before writing code; a pull request with no issue behind it is
closed. [CONTRIBUTING.md](CONTRIBUTING.md) has the policy, the build and the
commit convention.

## Licence

MIT (SPDX: `MIT`); see `LICENSE`. `NOTICE` carries the attribution for the
CLIProxyAPI ABI types this plugin mirrors.
