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

**A seat whose quota came back early still gets no requests.** The host keeps
the cooldown from the 429 the seat had until the reset that 429 named. When
that cooldown ends at the reset the plugin saw refused, the plugin ends it
within a poll or two of a usage read showing the quota back. The page warns
when it cannot: on a host older than 8.0.12, when the reset fails, or when the
host cools the seat again right after. Then, or for a cooldown the page does
not mention, reset the seat with
`POST /v0/management/reset-quota {"auth_index": …}`, or restart the host,
which drops every cooldown unless
`routing.cooldown.save-cooldown-status` (`save-cooldown-status` before 8.0) is
on.

**Seats show as ineligible.** Their reading is missing, older than
`max-staleness`, or has never succeeded. The page names the reason per seat
and carries the poll error; wait one `poll-interval` or click Sync now on the
status page.

**The page warns that the usage endpoint is throttled.** The endpoint limits
how often one caller reads it, across every seat at once, so a sweep that
lands soon after another is refused. Each seat keeps its last reading, and
routes on it until it is older than `max-staleness`; the next poll reads
again. A `poll-interval` shorter than the default, or Sync now pressed
repeatedly, makes this more likely.

**The config block seems ignored.** A block that does not parse loads the
plugin disabled with defaults, and the page warns that the plugin is disabled.
Fix the YAML; the host applies the fix live, without a restart.
