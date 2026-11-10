# Launch Wait Keepalive (#2953)

The Wave 9 exit review (#2386) found that a launch waiting for memory loses
its place when one production publication pass runs past the wait's
five-minute lease. The lane is serial, so it refuses a launch it waits on
again only when a pass reaches that launch's task, and one verification
command may run 45 minutes. A newer task's writer, asked for by the dispatch
loop meanwhile, then reserves first. This amends the "A wait lapses unless its
launch keeps being refused" bullet of
`devlog/2026-11-03-0946-memory-reservation-at-admission.md`: its assumption
that "a pursued launch refreshes it once a minute" doesn't hold for the
publication lane while it works on another task.

## Decisions

- **Chose a pass-scoped keepalive over relying on refusals alone.** While a
  publication pass runs, the engine writes a keepalive time about once a
  minute to the waits the lane may still be pursuing, and stops when the pass
  returns. The gate counts a wait as live while its latest refusal or
  keepalive is inside the lease. A daemon that is gone writes neither, so its
  waits still lapse.
- **The kept set is waits refused since the previous pass's start minus the
  lease** (this pass's start on the first pass after a daemon start). A wait
  the lane still pursues is refused again in every pass that reaches its task,
  so it stays in the set. One nothing pursues drops out after two passes at
  most and then lapses. A keepalive never revives a wait already lapsed when
  it is written: a pursued wait is kept every minute, well inside the lease,
  so the rule costs it nothing, and a dead one stays dead.
- **Chose a separate `kept_at` column over refreshing `refused_at`.**
  `refused_at` decides which waits the next pass keeps, and
  `recordLaunchOrder` throttles real refusal writes by it. A keepalive written
  there would carry itself into the next pass's set, so a dead wait would live
  for as long as passes keep running long.
- **The keepalive wraps the lane's pass in the engine, not the Tier A
  publication workflow.** It changes no Tier A file, and it writes through the
  internal write path, so it moves no client revision.

## Rejected

- **A per-lane heartbeat or per-kind liveness the gate reads.** The
  2026-11-03 note already rejected tying the gate to subsystem state; the gate
  still reads only the wait rows.
- **A longer lease.** It would have to outlast the longest verification, so
  every dead wait would block newer launches for 45 minutes or more.
- **Making the publication pass concurrent.** Far larger than the defect and
  out of scope.

## Accepted Bound

The kept set doesn't record which lane or kind owns a wait, so it also keeps
dispatch-loop execution waits. A dead wait of either kind can outlive the
lease by up to two passes. A lapse, early or late, still costs a wait only
its place in the order, never the budget.

Revisit when the lane's hold-retry pacing can skip a pursued task for longer
than the lease, which would let a live wait drop out of the kept set again.
