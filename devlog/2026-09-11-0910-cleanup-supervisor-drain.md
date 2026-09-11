# Bounded Rig Cleanup Survives the macOS Group-Kill Race

`real_work_bounded_rig` in `scripts/real-work-lifecycle.sh` could leave a
process behind after killing its supervisor process group, and that orphan
could hold the caller's output open for up to an hour. This note records the
race and the two-part fix. Context: issue #1290.

## The Race

On macOS a process-group `SIGKILL` can miss a group member that a member is
forking at that instant. The old supervisor wrote the rig result and then
forked `while :; do sleep 3600; done`. When the parent's single
`kill -KILL -- "-$supervisor"` landed together with that fork, the fresh
`sleep 3600` survived with parent pid 1, inherited the supervisor's
`trap '' TERM` (so it ignored TERM), and kept the supervisor's stdout and
stderr. A caller capturing cleanup output with `$(...)` then waited up to an
hour for that sleep to exit.

Evidence (macOS, bash 5.3, scratch reproduction, not committed):

- Pre-fix supervisor shape, single kill as soon as the result appears:
  reproduced the leak (this session: 21 of 400 runs; the #1290 investigation
  saw 132 of 400 under more load). The window is roughly 1 ms, so the hang is
  rare and load-dependent.
- Fixed supervisor shape and a group member forking during the kill, each with
  the drain: 0 of 400 leaked.

Linux was not reproduced. Linux script CI passed, consistent with the Linux
kernel restarting a fork interrupted by a group signal. That explanation is
unverified; Linux CI is the check.

## The Fix

Chose a two-part group discipline over widening the callers' timeouts or
switching cleanup off the process-group mechanism, because the guarantee to
keep is unchanged (cancel `freesided` first, then escalate against a
supervisor-reserved group) and the only defect is the kill's completeness.

1. **Reserve first, fork nothing after the result.** The supervisor starts a
   TERM-ignoring reservation subshell *before* it launches the rig command,
   then after writing the result blocks in `wait` on that reservation instead
   of forking a new sleep. With no fork racing the final kill, the supervisor
   cannot orphan a sleep. The reservation's own `trap '' TERM` lives inside its
   subshell, so the rig command still receives TERM normally. The whole
   supervisor subshell's stdout and stderr are redirected to
   `rig-cleanup.log`, so even a hypothetical escapee holds the log, never the
   caller's capture.

2. **Drain the group after the kill.** After `kill -KILL -- "-$supervisor"`,
   re-send the group KILL until a snapshot taken *after* a kill and a settle is
   empty, for at most ~1 s. A member that escaped the first signal is caught
   here; an unsettled empty reading is not trusted, because the escapee may not
   be visible the instant the kill returns. Zombies (Z state) are excluded,
   since they are already dead. Cleanup fails closed (prints a message, returns
   nonzero) if members remain after the bound, or if the process table cannot
   be read at all.

   The drain does not assume the group id stays ours. Bash reaps the killed
   leader asynchronously, before the final `wait`, so the leader pid (which is
   the group id) is freed during the loop. In principle that pid could be
   recycled as an unrelated group's leader and be signalled by the loop.
   Reaching it needs the OS to cycle its whole pid space inside the ~1 s drain,
   which does not happen at the scale this runs (an operator shell or CI, on
   macOS's 99998-pid space or a default Linux `pid_max`). Anchoring the leader
   to close this fully is not achievable in bash, which reaps background
   children on its own, and the caller-visible hang is already closed by part
   1's log redirect, so the residual is accepted rather than chased with a
   heavier mechanism.

The second part is the contract's one behavior addition: a group that cannot be
emptied now fails cleanup. Callers already treat nonzero as failure
(`run_rig_cleanup` preserves the stale manifest; the stale `recover` path and
`real-work-session.sh recover` refuse recovery), so no caller changed.

## Why `scripts/check-agent-image.sh` Is Excluded

It has the same single group KILL (lines 225 and 260), but its main shell
reaps the runtime leader while the watchdog kills it, so the orphan-holds-the-
pipe shape does not arise there. Carrying the drain into it, or recording why
it is unnecessary, is a separate unit. Follow-up: #1300.

## Rejected Options

- **Widen `rig_release_timeout`.** Treats the symptom (a longer bound) and
  still leaves a TERM-immune orphan on every miss.
- **Poll-and-reap instead of a reservation.** The reservation is what keeps the
  group non-empty across the TERM broadcast; without it the group can empty and
  its id be reused before the drain.
- **A committed stress test.** The race cannot fail deterministically (a ~1 ms
  window) and does not fire on Linux CI, so a committed test would be flaky or
  vacuous. The scratch harness stays out of the tree; the deterministic
  fixtures cover the observable properties (reservation-first, output routing,
  bounded cancel, drain failure).

## Revisit When

- The drain's `ps`/zombie filter behaves differently on a supported platform
  (a dead-but-unreaped child counted as live would fail cleanup for no reason).
- A production caller begins capturing cleanup output with a pipe, which would
  make any residual orphan a hang rather than a stray sleep.
- Retained sessions matter: sessions created before this fix keep their copied
  helper and the old race; the contract accepts that.
- The drain runs somewhere the pid space can realistically cycle within its
  ~1 s bound (extreme process churn against a very small `pid_max`), which would
  make the accepted pid-reuse residual reachable and call for a leader anchor.
