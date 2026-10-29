# Privacy and data

The plugin calls one external endpoint, the usage endpoint at `usage-url` set
in [Configuration](configuration.md), through the host's HTTP client with each
seat's OAuth token, which the host already holds.

The status page leaves out account addresses, file paths and network
addresses, so it is safe to put on a screen. An email is masked to its first
letter and its domain, and a `note` appears as written. Credential ids,
conversation keys and the ids that tell login teams apart are keyed by a
secret in `page-id.key` beside
`history.json`; deleting that file changes every published id. The `status`
management route, listed in [Status page](status-page.md), serves everything
unreduced, behind the same key.

On macOS and Linux, turning on `web.socket` (off by default) while
`web.enabled` is on makes the plugin also serve a Unix socket, `api.sock`,
beside `history.json`. It answers `GET /api/seats?session=<Claude Code session
id>&model=<model>` with the seat that session is bound to and every seat's
windows, in the page's form: masked names, keyed-hash ids and the page's
warnings. It adds each seat's use per dollar, worked out from the token counts
on the responses the plugin sees, priced at list price. The plugin keeps the
dollar amounts as hourly totals per seat, in memory only, and reads the last
day of them. [Local API](local-api.md) describes the answer. It never listens
on the network: the plugin makes its directory the host user's alone (no group
or other permission, owned by that user, not a link) and the socket `0600`, so
only the user the host runs as can connect, the user who can already read
`history.json`. A failure to bind it is a status-page warning. Windows serves
no socket.

`~/.cli-proxy-api/plugins/claude-seat-pacer/history.json` keeps per-seat
utilization samples and each seat's last reading across restarts: credential
ids (file names or account emails), timestamped fractions, each window's reset
time, when the provider refused each window and what ended each refusal, and
when the plugin saw each seat's current login made, with a digest of the
device id that login left. To tell a seat's login, the plugin reads four
fields of its credential file, `expired`, `last_refresh`, `organization_name`
and `claude_device_ids`; the device id is random, so its digest reveals
nothing. The team name appears on the status page so you know which
organization to pick when logging in again, with any email address in it
masked as the seat's is.
An unreadable file is set aside as `history.json.corrupt-<unix-ts>` and a
fresh one starts. No token, refresh token or request body is ever logged, and
an error that would carry a token is scrubbed first.
