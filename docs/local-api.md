# Local API

The local API lets a program on the same machine ask which seat a Claude Code
session is on and how much of each seat's quota is used. It is read-only and off
by default, and it serves only the masked figures the status page shows: no
token, credential file name or email. File permissions control access instead
of the management key: it answers on a Unix socket that only the host's user
can open, and never listens on the network. Use it for a Claude Code status
line, or for a Claude Code mod that shows the seat in Claude Desktop. It is
available on macOS and Linux only.

## Turn it on

```yaml
plugins:
  configs:
    claude-seat-pacer:
      web:
        socket: true
```

`web.enabled` must be on too, as it is by default. The plugin binds
`api.sock` beside `history.json`, at
`~/.cli-proxy-api/plugins/claude-seat-pacer/api.sock` in the home of the
user the host runs as. Only that user can connect; [Privacy and
data](privacy.md) says how. When the plugin cannot bind the socket, the status
page says why, and the plugin tries again on its next usage read.

## Read it

```sh
curl -s --unix-socket ~/.cli-proxy-api/plugins/claude-seat-pacer/api.sock \
  'http://localhost/api/seats?model=claude-opus-5-5'
```

The socket serves `GET` and `HEAD` on `/api/seats` and nothing else: any other
path is a 404, and any other method a 405. It takes these query parameters,
all optional:

| Parameter | Meaning |
|---|---|
| `session` | A Claude Code session id. The answer's `binding` names the seat that session is on. |
| `model` | The model the seats are scored for. A bracketed suffix such as `[1m]` is dropped, as Claude Code drops it from its requests. With no model, the model the plugin last routed, or `claude-opus-5` before its first request. |
| `since` | An RFC 3339 instant within the last 8 days. Each window then carries `used_since`. Write a `+` offset as `%2B`. |
| `since_five_hour` | The same, for the 5-hour window in place of `since`. |

The plugin answers from memory and makes no request of its own. Its figures
change when it reads usage, every `quota.poll-interval` (2 minutes by
default), and when a response passes through it, so asking more often than
that returns the same figures.

## The answer

```json
{
  "version": 1,
  "now": "2026-10-28T00:56:30-04:00",
  "polled_at": "2026-10-28T00:56:30-04:00",
  "model": "claude-opus-5-5",
  "binding": {
    "auth_id": "6d45edc91db7dbf3",
    "model": "claude-opus-5-5",
    "last_seen": "2026-10-28T00:56:25-04:00",
    "expires_at": "2026-10-28T01:56:25-04:00"
  },
  "next": "6d45edc91db7dbf3",
  "seats": [
    {
      "auth_id": "6d45edc91db7dbf3",
      "label": "seat-1",
      "priority": 10,
      "host_status": "active",
      "eligible": true,
      "slack": 0.043,
      "weekly_per_usd": 0.00001,
      "five_hour_per_usd": 0.0001,
      "windows": [
        { "kind": "five_hour", "utilization": 0.31, "resets_at": "2026-10-28T01:36:30-04:00", "used_since": 0.155 },
        { "kind": "seven_day", "utilization": 0.35, "resets_at": "2026-11-01T11:56:30-05:00" },
        { "kind": "weekly_scoped", "scope": "Fable", "utilization": 0.28, "resets_at": "2026-11-01T11:56:30-05:00" }
      ]
    }
  ],
  "warnings": []
}
```

Seats are named as the status page names them: `auth_id` is the page's id for
a seat, a keyed hash, never its credential file name or email, and `label` is
the name the page shows. A field that does not apply is left out.

| Field | Meaning |
|---|---|
| `version` | The format, 1. It rises when a field goes away or changes meaning, and not when a field is added or a field below gains a new value, so ignore fields and values you do not know. |
| `now` | When the plugin answered. |
| `polled_at` | About when the plugin last read every seat's usage. |
| `model` | The model the seats were scored for. |
| `binding` | The seat the session is on for `model`. A session holds one seat per model; with none for `model`, this is the one it used last. `null` when `session` is missing or the session holds no seat. |
| `binding.model` | The model the binding is for. |
| `binding.last_seen` | When the session last sent a request on it. |
| `binding.expires_at` | When the binding ends if the session sends no further request: `affinity.ttl` after `last_seen`. For a binding to another model it is `last_seen` itself, since the session's next request on `model` gets a fresh pick. |
| `next` | The seat a new conversation on `model` goes to. Absent when no seat can take one. |
| `seats[].priority` | The seat's priority. Only the highest priority among the seats the host offers takes conversations; see [How it picks](how-it-picks.md). |
| `seats[].host_status` | The credential's state as the host reports it, such as `active`, `error`, `unavailable` or `disabled`. The status page hides a `disabled` seat. |
| `seats[].eligible` | Whether the seat can take a new conversation on `model`. |
| `seats[].reason` | Why it cannot: `spent`, `provider-rejected`, `stale-snapshot`, `no-snapshot`, `fetch-failed`, `no-bearing-window`, `bad-reading` or `not-a-candidate`. |
| `seats[].slack` | How far the seat's 7-day use is behind its plan, as a fraction of the window; negative when it is ahead. |
| `seats[].weekly_per_usd` | How much of the 7-day window one dollar of list-price spend used on this seat lately, as a fraction. Multiply a session's own list-price cost by it to estimate that session's share of the window. Absent until the plugin has seen the seat's use rise with its spend. |
| `seats[].five_hour_per_usd` | The same for the 5-hour window. |
| `windows[].kind` | `five_hour`, `seven_day`, or `weekly_scoped` for a model family's weekly cap. |
| `windows[].scope` | The model family of a `weekly_scoped` window, as the usage endpoint names it, such as `Opus`, `Sonnet` or `Fable`. |
| `windows[].utilization` | How much of the window is used, as a fraction; above 1 past the cap, `null` for a reading that is not a number. |
| `windows[].resets_at` | When the window resets. |
| `windows[].used_since` | How much of the window was used after `since` (or `since_five_hour`), when the plugin's history covers the window. |
| `warnings` | The status page's warnings. |

## A Claude Code status line

Claude Code passes the session id and model to its status line command, so
the status line can show the session's seat. This script needs `curl` and
`jq`, and prints nothing when the socket is off:

```sh
#!/bin/sh
# ~/.claude/seat-status.sh
sock="$HOME/.cli-proxy-api/plugins/claude-seat-pacer/api.sock"
input=$(cat)
session=$(printf '%s' "$input" | jq -r '.session_id // empty')
model=$(printf '%s' "$input" | jq -r '.model.id // empty')
curl -sg --max-time 1 --unix-socket "$sock" \
  "http://localhost/api/seats?session=$session&model=$model" |
  jq -r '
    def used(k): [.windows[] | select(.kind == k and .utilization != null)
      | "\(.utilization * 100 | round)%"] | first // "–";
    (if .binding then "" else "next " end) as $pre
    | (.binding.auth_id // .next) as $id
    | .seats[] | select(.auth_id == $id)
    | "\($pre)\(.label)  5h \(used("five_hour"))  7d \(used("seven_day"))"'
```

Make it executable and point `~/.claude/settings.json` at it:

```json
{ "statusLine": { "type": "command", "command": "~/.claude/seat-status.sh" } }
```

It shows `seat-1  5h 31%  7d 35%`, or `next seat-1 …` before the session's
first request.

## A Claude Code mod

Claude Desktop runs Claude Code sessions, so a Claude Code mod can draw the
same figures in a band above the prompt box. A mod reaches the socket with
`$.http.fetch` and its `socketPath` option. To show a session's share of each
window, it can keep the session's cost over time and multiply each stretch by
the seat's `weekly_per_usd` and `five_hour_per_usd`.
