# cpa-plugin-claude-seat-pacer

**Claude Seat Pacer** is a CLIProxyAPI plugin that routes requests across
multiple Claude OAuth subscription seats by burn pace, while keeping each
conversation pinned to one seat so Anthropic prompt caches keep hitting.

The plugin id is `claude-seat-pacer`. Configuration lives under that key:

```yaml
plugins:
  configs:
    claude-seat-pacer:
      enabled: true
```

Set `routing.session-affinity: false` alongside it. This plugin owns
affinity, and a plugin pick never seeds the host's cache, so both cannot
own it.

Utilization history is kept in
`~/.cli-proxy-api/plugins/claude-seat-pacer/history.json`.

Status: under construction. See AGENTS.md for the design contract.
