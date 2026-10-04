# How it picks

Every new conversation goes to the eligible seat furthest behind its weekly
plan, and then stays on that seat.

Each weekly window's plan rises from nothing at the window's start. At the
default `landing-target: 1.10` on the linear shape it reaches the full quota
about nine-tenths of the way through the week. Any quota a seat still holds
near its reset puts it behind its plan, at 1.10 or at 1.0. How far behind a
seat is counts the all-models weekly window in full and a model-family window
at half. The 5-hour window can make a seat ineligible but carries no weight by
default: it resets several times a day and nothing in it carries over.

Sending everything to the seat that resets soonest looks only at the clock: it
keeps feeding a nearly spent seat while one with most of its week unused
waits. A fixed usage cutoff looks only at the meter, and wastes whatever sits
above the cutoff. A plan per weekly window weighs both. It says how much a
seat should have used by now, and the seat furthest behind its plan is the
one most at risk of reaching its reset with quota left.

The defaults suit my pool: each seat's plan reaches full quota about 15 hours
before its reset. Set `landing-target: 1.0` to plan each seat to finish at its
reset instead. For an even spread, disable the plugin; the host's own strategy,
round-robin by default, takes over.

A seat is ineligible when:

- Anthropic has refused a request on a window covering the requested model's
  family,
- such a window reads full, or not as a number,
- no window covers the requested model's family,
- its usage read has never succeeded, or
- its reading is older than `max-staleness`.

A seat's reading comes from two sources. The usage endpoint reports the 5-hour
window, the weekly all-models window and each model family's weekly window;
the plugin reads it every `poll-interval`, two minutes by default, and again a
few seconds after each reset. Between reads, the rate-limit headers on every
completed response update the 5-hour, weekly and Fable weekly windows and
record any refusal, so the seat taking new conversations closes its gap as it
works, and once another seat is further behind its own plan, new conversations
go there. A seat refused or full on a window takes no new conversation for the
models that window caps while another seat has room, until a later reading
shows room on it.

CLIProxyAPI's own session affinity never binds a conversation a plugin routes,
so the plugin binds each one itself. A new subagent on the same model joins
its parent's seat, even a full one, unless that seat has refused the model.

Once bound, a conversation:

- stays on its seat while the host still offers it, and with the default
  `override-threshold` even when another seat is further behind or its own
  runs out, because the cache hit is worth more than the rebalance;
- moves when a request on its seat fails, a refusal included, and the host
  retries it on another seat, since the retry no longer offers the failed one;
- otherwise moves after a refusal for its model only when another seat can
  take that model;
- loses its binding once idle past `affinity.ttl`: the binding stops counting
  as live at once and is cleared after the next background poll.

Bindings are per model, so a conversation that switches model is placed
separately for the new one. They live in memory, so a host restart, a new
plugin version, or a change to `affinity.ttl` or `max-sessions` starts with
none.

When no seat is eligible, a conversation still gets one stable seat: first one
Anthropic has neither refused nor reported full, then the one with the fewest
live conversations. A request with no session id and no eligible seat goes to
the host's own selector. A request the host pins to one seat leaves its
conversation's binding alone.
