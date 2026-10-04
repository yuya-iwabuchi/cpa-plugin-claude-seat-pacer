# Install

The host must be CLIProxyAPI 7.2.145 or newer; the plugin is tested against
7.3.10, 7.3.15 and 8.0.4.

## From the Plugin Store

Claude Seat Pacer is in CLIProxyAPI's official plugin store, so there is no
source to add.

1. Turn plugins on (`plugins.enabled: true`; see
   [Configuration](configuration.md)).
2. Install Claude Seat Pacer from the Plugin Store in the Management Center. It
   checks the download against the release's checksums and loads it without a
   restart.
3. Click Update when a new release shows as one, within about an hour of its
   publication.

If you installed it from the author's
[registry](https://github.com/yuya-iwabuchi/cpa-plugin-registry) before it
joined the official store, it keeps updating from there as long as that
registry stays in your plugin sources. Moving to the official store means
deleting the plugin in the Management Center, which drops its saved settings
and needs a host restart while the plugin is loaded, then installing it again.

The store records the installed version under `store:` in the plugin's config
block. The host then loads only that version and deletes the plugin's other
library files at its next start, so remove that key before installing by
hand.

## Other ways to install

Download your platform's zip from the [latest
release](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/releases/latest)
and put the library in `~/.cli-proxy-api/plugins/<goos>/<goarch>/`, keeping
its file name. The libraries load on macOS 12, Windows 10, or Linux with glibc
2.34, or newer: on Apple silicon Macs, Windows and Linux from v0.1.1, and on
Intel Macs from v0.2.1. [SECURITY.md](../SECURITY.md) shows how to verify the
zip.

Or build from source, with Go 1.25 and a C toolchain. On an Intel Mac the
build first compiles a patched Go toolchain from source, which takes a few
minutes:

```sh
git clone https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer
cd cpa-plugin-claude-seat-pacer
make install   # PLUGIN_DIR overrides ~/.cli-proxy-api/plugins
```

A plugin installed by hand stays off until its config block enables it: set
`plugins.configs.claude-seat-pacer.enabled: true`, as
[Configuration](configuration.md) shows.

The host loads a library the next time it applies a config change, but only
from a file path it hasn't loaded before. After replacing a loaded file,
restart the host (`brew services restart cliproxyapi` on Homebrew).

## Updating and uninstalling

Uninstall by deleting the plugin in the Management Center. A new version
starts with no conversation bindings, costing each open conversation one
prompt-cache miss. On macOS, restart the host after uninstalling: an unloaded
Go library can leave a thread running in the host.
