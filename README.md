# <img src="docs/logo.svg" width="28" height="28" alt=""> Claude Seat Pacer

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin for pooling several Claude subscriptions. It spends each seat's weekly quota before the reset instead of letting it lapse, and keeps each conversation on one seat so its prompt cache keeps hitting.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/hero-dark.png">
  <source media="(prefers-color-scheme: light)" srcset="docs/hero-light.png">
  <img src="docs/hero-light.png" width="900" alt="Screenshot of the Claude Seat Pacer status page, showing the live bar naming the seat the next new conversation will land on, three seats with their rank and cost, the one taking the next conversation highlighted, and the weekly pace plot with the cost-ordered seat list beside it.">
</picture>

## Why

A Claude subscription has a 5-hour limit and a weekly limit, and weekly quota
left at the reset is gone. More seats raise the ceiling, and CLIProxyAPI, a
proxy that puts several subscription logins behind one API endpoint, makes
pooling them easy. Its fill-first and round-robin strategies don't track how
far each seat is into its week, though, so a seat can reach its reset with
quota to spare while work goes to seats whose weeks have days left.

Moving a conversation has a price too. Anthropic's prompt cache never crosses
organizations, and a long conversation's input is nearly all cache reads, so a
conversation moved to another seat pays full price for everything it has
already sent.

I run several Claude Code sessions at once across a few seats, and wanted a
pick that spends every seat's week and never moves a conversation without a
reason. This plugin is that pick.

## How it works

- **Every seat gets a plan for its week.** The plan rises from nothing at the
  window's start to the full quota a little before its reset. A seat resetting
  in a day with 5% left is on track; one resetting in three days with 70% left
  is behind.
- **The seat furthest behind takes the next conversation.** A seat still
  holding quota near its reset goes first, and one that just reset waits until
  the others are closer to their plans.
- **Conversations stay on their seat.** Each conversation, and its subagents
  on the same model, keeps its seat. It moves only if that seat refuses it,
  fails a request or drops out of the host's rotation, or after an hour idle.
- **It sends no probe requests.** Each seat's reading comes from Anthropic's
  usage endpoint, read every two minutes, and from the rate-limit headers on
  every response your own requests get. The usage read is its only request of
  its own.
- **It declines rather than breaks.** The pick reads memory only and never
  fails a request. When it can't help, the host's own selector routes the
  request.

## See the pool

The plugin adds a status page to the Management Center showing the next pick,
every seat against its plan, the live bindings and the routing log. Over time
piles every seat's use into the pool's total and totals each day, so you can
see whether the pool has enough seats.

<img src="docs/over-time-stacked.png" width="900" alt="The Over time chart in its Stacked form over two weeks of 7d Standard: three seats' use piled into the pool's total under the 300% line of what the pool holds, a bracket over each day with the pool's use, each seat's share inside its band, reset marks where a seat's band falls, and past now each band held until its seat's reset; beside it, what the pool has left and what each reset brings back.">

## Install

You need CLIProxyAPI 7.2.145 or newer; the plugin is tested against 7.3.10,
7.3.15, 8.0.4, 8.0.13, 8.0.15 and 8.0.20.

1. Turn plugins on and set a management key, which the status page asks for:

   ```yaml
   management:
     secret-key: "<a long random string>"
   plugins:
     enabled: true
     dir: "~/.cli-proxy-api/plugins"
   ```

   Before CLIProxyAPI 8.0, `management` is `remote-management`.
2. Install Claude Seat Pacer from the Plugin Store in the Management Center.
   It loads without a restart.
3. Keep the seats you want paced together on one `priority`. They share one
   by default.

Every setting has a default. [Configuration](docs/configuration.md) lists them,
and [Install](docs/install.md) covers manual installs, building from source,
updating and uninstalling.

## Privacy

The plugin sends each seat's token only to Anthropic's usage endpoint, or to a
loopback host you set as `usage-url`, through the host's own HTTP client, and
never logs a token or a request body. The status page masks email addresses,
so it is safe to put on a screen. [Privacy and data](docs/privacy.md) covers
what it stores.

## Documentation

- [How it picks](docs/how-it-picks.md): when a seat is eligible, how it is
  ranked, and when a conversation moves.
- [Status page](docs/status-page.md): every section of the page, and its
  routes.
- [Configuration](docs/configuration.md): every setting and its bounds.
- [Install](docs/install.md): the Plugin Store, manual installs, building from
  source, updating.
- [Privacy and data](docs/privacy.md): what the plugin sends, shows and
  stores.
- [Troubleshooting](docs/troubleshooting.md): the failures seen so far.

## Contributing

Bug reports and questions go to
[Issues](https://github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/issues).
Open an issue before writing code; [CONTRIBUTING.md](CONTRIBUTING.md) has the
policy, the build and the commit convention.

## Licence

MIT; see `LICENSE`. `NOTICE` carries the attribution for the CLIProxyAPI ABI
types this plugin mirrors, and `THIRD_PARTY_NOTICES` the licences of the code
the library links statically. Each release zip carries all three.
