# Claude Seat Pacer

Uses up each Claude seat's weekly quota before it resets, and keeps every conversation on one seat so its prompt cache keeps working.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/hero-dark.png">
  <source media="(prefers-color-scheme: light)" srcset="docs/hero-light.png">
  <img src="docs/hero-light.png" width="900" alt="Screenshot of the Claude Seat Pacer status page, showing the live bar naming the seat the next new conversation will land on, three seats ranked by pace score with the winner highlighted, and the weekly pace plot with the cost-ordered seat list beside it.">
</picture>

## Why

A Claude subscription has a 5-hour limit and a weekly limit, and any weekly
quota left at the reset is lost. More seats raise that ceiling linearly, and
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) makes pooling them
easy. Its fill-first and round-robin strategies and session affinity work well
out of the box, but they don't track how far each seat is into its week. I
wanted a simple, usage-aware pick: drain the seats closest to their reset and
hold back the ones that just reset.

Claude Seat Pacer gives each seat a steady plan that finishes a little before
its reset, and sends every new conversation to the seat furthest behind its
plan. Once a conversation lands on a seat it stays there. Anthropic's prompt
cache never crosses accounts, and about 96% of input tokens are cache reads,
so moving a live conversation would pay full price for everything it has
already sent.

The defaults suit my pool: drain the seat about to reset. Set
`landing-target: 1.0` to plan each seat to finish at its reset instead of
early. For an even spread, disable the plugin and the host's round-robin takes
over.

## Install

The host must be CLIProxyAPI 7.2.145 or newer; the plugin is tested against
7.3.10 and 7.3.15.

Download your platform's zip from the
[latest release](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/releases/latest)
and put the library in `~/.cli-proxy-api/plugins/<goos>/<goarch>/`, keeping its
file name. From v0.1.1 the libraries load on macOS 12, Windows 10, or Linux
with glibc 2.34, or newer. [SECURITY.md](SECURITY.md) shows how to verify the
zip.

To build from source instead, with Go 1.25 and a C toolchain:

```sh
git clone https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer
cd cpa-plugin-claude-seat-pacer
make install   # PLUGIN_DIR overrides ~/.cli-proxy-api/plugins
```

The host loads a library the next time it applies a config change, but only
from a file path it hasn't loaded before. After replacing a loaded file,
restart the host (`brew services restart cliproxyapi` on Homebrew). A new
version starts with no conversation bindings, costing each open conversation
one prompt-cache miss. On macOS, also restart after uninstalling: an unloaded
Go library can leave a thread running in the host.

## Configure

```yaml
host: "127.0.0.1"          # optional: keeps the proxy off the network

remote-management:
  secret-key: "<a long random string>"   # the status page asks for this

routing:
  session-affinity: false

plugins:
  enabled: true
  dir: "~/.cli-proxy-api/plugins"
  configs:
    claude-seat-pacer:
      enabled: true
```

- CLIProxyAPI ships with plugins off. `dir` expands a leading `~/`; a relative
  path resolves against the host's working directory, which isn't your home
  directory when the host runs as a service.
- The status page reads through the management API, which the host serves only
  once `remote-management.secret-key` is set.
- Turn the host's session affinity off: the plugin owns affinity, and a
  plugin's pick never seeds the host's cache.
- Give every seat the same `priority`. The host offers the plugin only its top
  tier, so a seat alone there takes every new conversation.
- A seat is named by its credential's `note` (set on the auth-file card in the
  Management Center, or as a `note` key in the file), or else by its account
  email, masked to `y…@example.com`.

### All settings

Every key is optional; `landing-target`, `affinity.ttl`, `override-threshold`
and `poll-interval` are the ones worth tuning. The host applies changes live.
Changing `affinity.ttl` or `max-sessions` clears the bindings, costing each
open conversation one prompt-cache miss.

```yaml
      providers: [claude]       # provider keys the plugin routes; others fall to the host
      models: []                # model ids the plugin routes; empty means all
      affinity:
        enabled: true
        ttl: 1h                 # idle time before a conversation's binding expires
        subagents: true         # a subagent shares its parent conversation's seat
        override-threshold: true # keep a binding even when another seat is further behind its plan
        max-sessions: 65536     # bindings held before the least recently seen is dropped
      pace:
        shape: linear           # linear, power or sigmoid
        curve-exponent: 1.0     # power shape only, and selects it when shape is unset; above 1 holds back early
        steepness: 8.0          # sigmoid shape only
        landing-target: 1.10    # 1.10 finishes ~9% early (linear), 1.0 at the reset; 0 < x <= 4
        weekly-weight: 1.0      # weight of the all-models weekly window
        scoped-weight: 0.5      # weight of a model-family weekly window
        session-weight: 0       # weight of the 5-hour window; it is a rate limit, not a budget
        hysteresis-margin: 0.05 # cost gap a challenger must beat to move a binding when override-threshold is off
      quota:
        poll-interval: 2m       # usage-endpoint read cadence; below 30s falls back to the default
        request-timeout: 10s    # one usage read; at most 1m
        max-staleness: 15m      # a reading older than this makes the seat ineligible; at least twice poll-interval
        persist-history: true   # keep utilization history across host restarts
        usage-url: https://api.anthropic.com/api/oauth/usage
      web:
        enabled: true           # serve the status page
        history-limit: 500      # routing decisions kept for the page; 1 to 10000
```

Durations take Go syntax (`30s`, `2m`, `1h`). A weight above 100, a
`request-timeout` above 1m or a `history-limit` above 10000 runs at that bound,
and the status page says so. A negative weight or `hysteresis-margin` counts as
0, and any other out-of-range value falls back to its default. `usage-url`
receives every seat's token, so it must be `https`, or `http` to a loopback
host; anything else runs at the default, with a status-page warning. A block
that doesn't parse loads the plugin disabled.

The Management Center saves each field as a top-level dotted key, such as
`affinity.ttl: 2h`, which wins over the nested form. When the two disagree the
status page names the dotted key; remove one.

## How it picks

Each weekly window's plan rises from nothing at the window's start. At the
default `landing-target: 1.10` on the linear shape it reaches the full quota
about nine-tenths of the way through the week, so any quota a seat still holds
near its reset puts it behind, even at `1.0`. How far behind a seat is counts
the all-models weekly window in full and a model-family window at half. The
5-hour window can make a seat ineligible but carries no weight by default: it
resets several times a day and nothing in it carries over.

A seat is ineligible when:

- Anthropic has refused a request on a window covering the requested model's
  family,
- such a window reads full, or not as a number,
- no window covers the requested model's family,
- its usage read has never succeeded, or
- its reading is older than `max-staleness`.

Once bound, a conversation:

- stays on its seat while the host still offers it, and with the default
  `override-threshold` even when another seat is further behind or its own
  runs out, because the cache hit is worth more than the rebalance;
- moves after a refusal for its model only when another seat can take that
  model;
- loses its binding once idle past `affinity.ttl`: the binding stops counting
  as live at once and is cleared after the next background poll.

When no seat is eligible, a conversation still gets one stable seat: first one
Anthropic has neither refused nor reported full, then the one with the fewest
live conversations. A request with no session id and no eligible seat goes to
the host's own selector. A request the host pins to one seat leaves its conversation's
binding alone.

## Status page

The Management Center gains a "Claude Seat Pacer" entry showing each seat's
windows, utilization history, pace score and eligibility for a chosen model,
the live bindings per seat, recent decisions with their scores, and a warning
for anything that leaves the plugin inert or degraded.

The page, at `/v0/resource/plugins/claude-seat-pacer/index.html`, carries no
data. It asks once per browser tab for the management key and reads from the
plugin's management routes, which the host answers only with that key and,
unless `remote-management.allow-remote` is on, only from the same machine.

The routes under `/v0/management/plugins/claude-seat-pacer/` are `GET status`
(the full status as JSON, `?model=<id>`), `GET page-status` (the page's view,
while `web.enabled` is on), `POST refresh`, `POST unbind?auth_id=` and
`POST bindings/sweep`.

## Privacy and data

The plugin calls one external endpoint, `usage-url`, through the host's HTTP
client with each seat's OAuth token, which the host already holds.

The status page leaves out account addresses, file paths and network
addresses, so it is safe to put on a screen. An email is masked to its first
letter and its domain, and a `note` appears as written. Credential ids are
keyed by a secret in `page-id.key` beside `history.json`; deleting that file changes every published
id. The `status` route serves everything unreduced.

`~/.cli-proxy-api/plugins/claude-seat-pacer/history.json` keeps per-seat
utilization samples across restarts: credential ids (file names or account
emails) and timestamped fractions. An unreadable file is set aside as
`history.json.corrupt-<unix-ts>` and a fresh one starts. No token, refresh
token or request body is ever logged, and an error that would carry a token is
scrubbed first.

## Troubleshooting

**The host does not list the plugin.** Plugins are off by default: set
`plugins.enabled: true` and point `plugins.dir` at the directory above
`<goos>/<goarch>/`. The plugin id comes from the library filename minus
its extension and `-v<version>`, and the config block, routes and history
directory all carry it, so keep the library's file name.

**The page keeps asking for the key.** The key is kept per browser tab. A 401
means the host did not accept the key; a 403 names its reason, either remote
management being off for a connection from another machine, or the address
being locked out for 30 minutes after five failed tries, a lock the Management
Center shares.

**The page says the management API is off (HTTP 404).** The host serves the
management API only once `remote-management.secret-key` is set. Set it,
restart the host, and enter that key on the page.

**Every new conversation lands on one seat.** That seat is alone on the top
`priority` tier. The status page warns and names the seats.

**Conversations do not stick.** The host's `routing.session-affinity` is still
on; set it to `false`. Nothing the plugin receives distinguishes the two
states, so the page cannot warn about this one.

**Seats show as ineligible.** Their reading is missing, older than
`max-staleness`, or has never succeeded. The page names the reason per seat
and carries the poll error; wait one `poll-interval` or `POST refresh`.

**The config block seems ignored.** A block that does not parse loads the
plugin disabled with defaults, and the page warns that the plugin is disabled.
Fix the YAML; the host applies the fix live, without a restart.

## Contributing

Bug reports and questions go to
[Issues](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/issues).
Open an issue before writing code; [CONTRIBUTING.md](CONTRIBUTING.md) has the
policy, the build and the commit convention.

## Licence

MIT; see `LICENSE`. `NOTICE` carries the attribution for the CLIProxyAPI ABI
types this plugin mirrors, and `THIRD_PARTY_NOTICES` the licences of the code
the library links statically. Each release zip carries all three.
