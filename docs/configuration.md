# Configuration

The plugin's settings sit under `plugins.configs.claude-seat-pacer` in
CLIProxyAPI's config file. A Plugin Store install writes that block with
`enabled: true`; a manual install needs it written by hand.

```yaml
server:
  host: "127.0.0.1"        # optional: keeps the proxy off the network

management:
  secret-key: "<a long random string>"   # the status page asks for this

plugins:
  enabled: true
  dir: "~/.cli-proxy-api/plugins"
  configs:
    claude-seat-pacer:
      enabled: true
```

- This is the layout CLIProxyAPI 8.0 introduced. Before 8.0, `server.host` is
  a top-level `host` and `management` is `remote-management`, and 8.0 still
  reads that layout.
- CLIProxyAPI ships with plugins off. `dir` expands a leading `~/`; a relative
  path resolves against the host's working directory, which isn't your home
  directory when the host runs as a service.
- The status page reads through the management API, which the host serves only
  once `management.secret-key` is set (`remote-management.secret-key` before
  8.0).
- The host offers each request only to the highest-priority credentials it
  can currently use, and the plugin paces conversations across the seats in
  that tier. Give the seats you want paced together the same `priority`; a
  seat on a lower priority takes requests only while no seat above it can
  take them.
- A seat is named by its credential's `note` (set on the auth-file card in the
  Management Center, or as a `note` key in the file), or else by its account
  email, masked to `y…@example.com`.

## All settings

These keys go under `claude-seat-pacer:`. Every key is optional; `landing-target`, `affinity.ttl`, `override-threshold`
and `poll-interval` are the ones worth tuning. The host applies changes live.
Changing `affinity.ttl` or `max-sessions` clears the bindings, costing each
open conversation one prompt-cache miss.

```yaml
      providers: [claude]       # provider keys the plugin routes; others fall to the host
      models: []                # model ids the plugin routes; empty means all
      affinity:
        enabled: true
        ttl: 1h                 # idle time before a conversation's binding expires
        subagents: true         # a subagent on the same model shares its parent conversation's seat
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
and the status page says so, as it does when a `max-staleness` you set is under
twice `poll-interval` and runs at twice it. A negative weight or
`hysteresis-margin` counts as 0, and any other out-of-range value falls back to
its default. `usage-url` receives every seat's token, so it must be `https` to
`api.anthropic.com`, or `http` to a loopback host; anything else runs at the
default, with a status-page warning. A block that doesn't parse loads the
plugin disabled.

The Management Center saves each field as a top-level dotted key, such as
`affinity.ttl: 2h`, which wins over the nested form. When the two disagree the
status page names the dotted key; remove one.
