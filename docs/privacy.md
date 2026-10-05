# Privacy and data

The plugin calls one external endpoint, the usage endpoint at `usage-url` set
in [Configuration](configuration.md), through the host's HTTP client with each
seat's OAuth token, which the host already holds.

The status page leaves out account addresses, file paths and network
addresses, so it is safe to put on a screen. An email is masked to its first
letter and its domain, and a `note` appears as written. Credential ids and
conversation keys are keyed by a secret in `page-id.key` beside
`history.json`; deleting that file changes every published id. The `status`
management route, listed in [Status page](status-page.md), serves everything
unreduced, behind the same key.

`~/.cli-proxy-api/plugins/claude-seat-pacer/history.json` keeps per-seat
utilization samples and each seat's last reading across restarts: credential
ids (file names or account emails), timestamped fractions, each window's reset
time, and when the provider refused each window and what ended each refusal. An unreadable file is set aside as
`history.json.corrupt-<unix-ts>` and a fresh one starts. No token, refresh
token or request body is ever logged, and an error that would carry a token is
scrubbed first.
