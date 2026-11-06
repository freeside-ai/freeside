# Darwin Zombie-Only Group Reads as Done on Cancel (#2692)

## Decision

- **Chose to map darwin's `EPERM` to `os.ErrProcessDone` only when a
  `kern.proc.pgrp` lookup after the kill finds every member a zombie, or
  none, over mapping `EPERM` unconditionally.** `EPERM` from a group kill
  also means a live member the daemon can't signal; on exit 0 an
  unconditional mapping would let that step read as passed. The lookup is
  the gate: a zombie can't fork, and `EPERM` means no signal was delivered,
  so the exit status `Wait` reports is the step's own. The lookup stays
  after the kill so a member that dies in between reads as dead, which it
  is by then.
- **Chose one exported `procbound.KillGroup` over two copies of the rule.**
  `Bind`'s `Cancel` and the verify room's `cancelProcessGroup` both call it.
  Converging `ProcRoom.Run` onto `Bind` stays its own unit. The
  `killgroup*.go` files join the Tier A list because the function decides
  whether a canceled verification step can read as passed.
- **Kept the rule to zombies, and deferred the mid-exit window
  (#2722).** The plan expected this fix to remove every `operation not
  permitted` room error from exit-0 steps. A probe showed a wider window
  before the zombie one: the leader has `P_WEXIT` set but `P_stat` is still
  `SRUN`, and the kill already returns `EPERM`. Of 300 `true` runs, the
  first `EPERM` sighting found that state 273 times and a zombie 27 times.
  It fails closed. Counting a `P_WEXIT` process as dead needs evidence that
  it can't still fork a live member, which this unit didn't establish.

## Refute-First Result

The claim to disprove: the new mapping lets a step that cancellation
killed, or that left a live member, read as passed. Checked by the author
and a fresh-context reviewer.

- **Disproved, signal delivered:** a probe of a zombie leader plus a live
  `sleep`, and of a zombie plus a live grandchild, got `nil` from the kill,
  not `EPERM`, so `KillGroup` returns `nil` and `Wait` reports `ctx.Err()`,
  a room error as before. `EPERM` came only when nothing was signalable.
- **Disproved, live member hidden from the lookup:** with the leader a
  zombie and a background `sleep` in the group, the lookup listed the sleep
  as `SRUN` and reported not done (`TestGroupOnlyZombiesSeesALiveMember`).
  A reused group ID shows a live member and fails closed.
- **Disproved, failed lookup reads as done:** a lookup error is joined to
  the `EPERM` and stays an error.
- **Disproved, stress probe** (not committed): 400 `ProcRoom.Run` calls
  each of `true`, `sh -c 'exit 3'`, and `sleep 1` under random 0 to 4 ms
  timeouts. No `exit 3` or `sleep` run read as passed. `operation not
  permitted` room errors on `true` fell from 42 to 9; the 9 are the
  mid-exit window above.
- **Allowed, not a regression:** a live member the daemon can't signal that
  exits or calls `setsid` between the kill and the lookup reads as done. It
  received no signal, so the exit code is real, and `setsid` already
  escapes any group kill.
- **Assumed:** `EPERM` from a darwin group kill means no member was
  signaled. Every probe agreed; the XNU source wasn't read.

Revisit when macOS changes `SZOMB` or what `kern.proc.pgrp` lists (the
zombie-window tests fail and production falls back to a room error), or
when #2722 settles the mid-exit window.
