# How it picks

Every new conversation goes to the eligible seat furthest behind its weekly
plan, and then stays on that seat. Seats with no weekly cap on the requested
model go first, paced against their 5-hour window instead.

Each weekly window's plan rises from nothing at the window's start. At the
default `landing-target: 1.10` on the linear shape it reaches the full quota
about nine-tenths of the way through the week, and keeps rising to 110% at the
reset. Any quota a seat still holds near its reset puts it behind its plan, at
1.10 or at 1.0, and past full the gap keeps growing as the reset nears, so a
seat with a little left that expires in hours goes ahead of a mid-week seat
further behind. `hold-at-full: true` holds the plan at 100% instead, which
scores a seat past that point by the quota it has left. A full seat is
ineligible either way. How far behind a
seat is counts the all-models weekly window in full and a model-family window
at half. On a seat with a weekly cap, the 5-hour window can make it
ineligible but carries no weight by default: it resets several times a day and
nothing in it carries over.

`shape: points` draws the plan through points of your own instead, for
example `points: ["0:0", "3:30", "5:100 straight", "7:110 straight"]`. Each
is a day of a 7-day week and the share of the quota the plan reaches by then,
and the curve runs into each point smoothly, in a straight line, as a step
that holds the earlier value until the point, or bent toward a guide:
`"5:100 via 4:20"` bends the stretch into day 5 toward day 4 at 20%, and
`pull 2` after the guide draws it closer (from 0.1, nearly straight, to 10;
1 when left out). A guide lies inside the box its stretch's two points span,
so the plan still never falls. A 5-hour window follows the
same curve scaled to its length. Days rise from point to point and the plan
never falls; a curve that starts after day 0 starts there at 0%, and one that
stops before day 7 holds its last value to it. The landing and
`hold-at-full` do not apply: the last point is the landing, and a flat run at
100% holds. A list the plugin cannot read runs the linear curve and raises a
warning on the status page.

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

Some seats, including some Team seats, report no all-models weekly window, only
a 5-hour window and a Fable weekly window. On a model outside Fable such a seat
has no weekly cap: its 5-hour window is its only quota, and it resets several
times a day. Every eligible seat with no weekly cap goes ahead of every seat
with one that is not boosted, and a seat with a weekly cap takes a new conversation for that model
only while no seat without one can. Once a seat with no weekly cap has room
again after its 5-hour reset, new conversations go back to it, and a
conversation already bound to a seat with a weekly cap stays there. Among seats
with no weekly cap, the one furthest behind its plan on the 5-hour window goes
first, whatever the weights; a seat whose 5-hour window has not opened yet is
neither behind nor ahead of its plan. On Fable these seats have a weekly cap
like any other and are paced the usual way. No setting controls this: it
follows each seat's latest readings, so a seat whose usage read or responses
report an all-models weekly window is paced like any other, and a pool in which
every seat has a weekly cap on the requested model is ranked on its weekly
pacing alone.

A seat whose usage read reports weekly windows and no 5-hour window, as a plan
with no 5-hour limit does, has no 5-hour limit to gate it. It is ranked on its
weekly windows alone, so it stays eligible while seats with a 5-hour limit wait
out a full one, though a seat with no weekly cap on the model still goes first.
With a `session-weight` above 0, the seats it is compared with carry a 5-hour
term it does not. A refusal of the 5-hour window it had before ends at the
first read that no longer reports the window, and the plugin then ends the
host's cooldown from that refusal as it does after an early reset.

You can boost a seat from the arrow (⤒) at the end of its row on the Seats
chart, which shows when you point at the row: for an hour,
for four hours, until its weekly reset (its 5-hour reset on a seat with no
weekly window), or until you clear it. While a boost lasts, the boosted seat
takes every new conversation it is eligible for, ahead of seats with no weekly
cap and of the pace ranking. Two boosted seats are ranked against each other as
usual. A boost moves no conversation that is already bound, and it never makes
an ineligible seat eligible: a full or refused seat takes nothing, boosted or
not. It also works only among the seats the host offers, which are the ones on
the highest `priority` in use. Boosts are kept in `boosts.state` beside the
history file, so they survive a plugin update or a host restart.

A seat is ineligible when:

- Anthropic has refused a request on a window covering the requested model's
  family,
- such a window reads full, or not as a number,
- no window covers the requested model's family,
- its usage read has never succeeded, or
- its reading is older than `max-staleness`.

A seat's reading comes from two sources. The usage endpoint reports each window
the seat has: the 5-hour window, the weekly all-models window and each model
family's weekly window; the plugin reads it every `poll-interval`, two minutes
by default, and again a few seconds after each reset. Between reads, the
rate-limit headers on every completed response update the 5-hour, weekly and
Fable weekly windows and record any refusal, so the seat taking new
conversations closes its gap as it works, and once another seat outranks it,
new conversations go there. A seat refused or full on a window takes no new
conversation for the models that window caps while another seat has room, until
a later reading shows room on it.

CLIProxyAPI's own session affinity never binds a conversation a plugin routes,
so the plugin binds each one itself. A subagent on the same model with no
live binding of its own joins its parent's seat, even a full one, unless that
seat has refused the model. Once bound, it keeps its own seat like any
conversation, even after its parent moves.

Once bound, a conversation:

- stays on its seat while the host still offers it, even when another seat is
  further behind or its own runs out, because the cache hit is worth more than
  the rebalance;
- moves when a request on its seat fails, a refusal included, and the host
  retries it on another seat, since the retry no longer offers the failed one;
- otherwise moves after a refusal for its model only when another seat can
  take that model;
- loses its binding once neither it nor, with `affinity.subagents` on, its
  subagents on the same model have sent a request for `affinity.ttl`: the
  binding stops counting as live at once and is cleared after the next
  background poll.

Bindings are per model, so a conversation that switches model is placed
separately for the new one. With `affinity.persist` on, the default, they
outlast a new plugin version and a host restart: the plugin saves the live ones
when the host stops it and reads them back when it starts. After a host
exits without stopping the plugin, the plugin restores only what the last stop
saved, and a change to `affinity.ttl` or `max-sessions` while the plugin runs
starts with none.

When no seat is eligible, a conversation still gets one stable seat: first one
Anthropic has neither refused nor reported full, then the one with the fewest
live conversations. A request with no session id and no eligible seat goes to
the host's own selector. A request the host pins to one seat leaves its
conversation's binding alone.
