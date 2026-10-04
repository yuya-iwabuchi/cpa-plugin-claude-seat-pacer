# Troubleshooting

**The host does not list the plugin.** Plugins are off by default: set
`plugins.enabled: true` and point `plugins.dir` at the directory above
`<goos>/<goarch>/`. The plugin id comes from the library filename minus
its extension and `-v<version>`, and the config block, routes and history
directory all carry it, so keep the library's file name.

**An Intel Mac host crashes or logs `cliproxy_plugin_init returned 1`.**
A library older than v0.2.1 shares the host's Go runtime state on Intel Macs
and corrupts it. Update the plugin. Any other Go plugin built with stock Go
does the same, and two plugins built with the same slot patch collide with
each other, so an Intel host runs this plugin beside no other Go plugin of
either kind.

**The page keeps asking for the key.** The key is kept per browser tab. A 401
means the host did not accept the key; a 403 names its reason, either remote
management being off for a connection from another machine, or the address
being locked out for 30 minutes after five failed tries, a lock the Management
Center shares.

**The page says the management API is off (HTTP 404).** The host serves the
management API only once `management.secret-key` is set
(`remote-management.secret-key` before 8.0). Set it, restart the host, and
enter that key on the page.

**Every new conversation lands on one seat.** Either that seat is alone on
the top `priority` tier, or the other seats on its tier, or every seat on the
tiers above it, are unavailable to the host or refused upstream. The status
page warns which, and names the seats when it is the tier layout.

**Seats show as ineligible.** Their reading is missing, older than
`max-staleness`, or has never succeeded. The page names the reason per seat
and carries the poll error; wait one `poll-interval` or click Sync now on the
status page.

**The config block seems ignored.** A block that does not parse loads the
plugin disabled with defaults, and the page warns that the plugin is disabled.
Fix the YAML; the host applies the fix live, without a restart.
