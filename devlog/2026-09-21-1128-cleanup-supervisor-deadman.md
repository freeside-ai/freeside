# The Cleanup Supervisor Is Now the Caller's Deadman

`real_work_bounded_rig` in `scripts/real-work-lifecycle.sh` used to leave its
supervisor process group running forever if the caller died first (SIGKILL, a
tool timeout, a closed terminal). The orphan (supervisor, its TERM-ignoring
reservation, the rig command, any `container` child) had no time limit and no
tie to the caller. This note records the revised discipline. Context: #1465.

## Revised Rule

The 2026-09-11 note (`cleanup-supervisor-drain.md`) established that the
supervisor "never exits on its own": it forked its reservation, then blocked in
`wait`, and a fallback `while :; do sleep 3600; done` guaranteed it never
returned. That rule is now reversed. The supervisor's reservation is the
caller's deadman:

- It watches the calling process and a wall-clock limit. When the caller is
  gone or the limit passes, it takes the recorded process group down itself.
- The supervisor no longer loops after `wait` returns; it exits, because the
  reservation can now end on its own.

The 2026-09-11 drain and reserve-first rules are unchanged. The macOS
group-kill race and its drain still stand; this only removes the "outlives the
caller forever" property, which was the orphan bug.

## Decisions

**Chose a self-killing reservation over widening caller timeouts or a separate
watchdog process.** The reservation already reserves the group and already
survives the drain; making it watch the caller reuses that structure and needs
no new process. A separate watchdog would be another thing to orphan.

**Caller pid is `$$`, not `$BASHPID`.** `$$` is the calling shell and works on
macOS's bash 3.2, which has no `$BASHPID`. `$$` is the top-level shell even
inside `$( … )`, so when the function is called in a command substitution the
deadman watches the whole script rather than the substitution subshell. That is
the safe direction: the group dies with the script, and the wall-clock limit is
the backstop for the rarer "only the substitution died" case. The three
production callers (`run-real-work.sh` cleanup at two sites,
`real-work-session.sh` recover) all call the function directly in the caller
shell, so `$$` is exactly the process whose death must end the group.

**The supervisor records the group to kill; the reservation does not compute
its own.** Getting a subshell's own process group id on bash 3.2 has no clean
way (no `$BASHPID`, no reliable self-pid trick). Instead the group is resolved
from a live member of it (the reservation) with `ps -o pgid=`, and written to a
flag file the reservation reads and kills. Crucially the *supervisor* writes it,
from inside its own subshell, not the parent: the supervisor is orphaned but
stays alive when the caller dies, so a caller SIGKILLed right after the fork
cannot leave the deadman disarmed (the refute-first pass below caught the
parent-writes-it version leaving exactly that hole). The group is recorded only
when distinct from the caller's group (captured while the caller is certainly
alive), so when job control made no new group the deadman stays disarmed and can
never signal the caller's own group where the rig holder runs.

**The limit is `2*bound + 60` seconds from supervisor start.** `2*bound` leaves
the cancellation its full budget (the bound is the per-attempt cleanup timeout;
doubling covers the TERM-then-KILL escalation) and the 60-second margin is
slack for a slow host. The margin is a judgment call, overridable for tests, and
kept a signed offset so a test can force an early limit. A suspended caller (a
sleeping laptop) can cross the limit and have its group ended early; the caller
then reports 124 and the harness marks the run `recovery-required`, which fails
safe.

**Parent group signals are guarded.** Because the group can now end on its own
before the parent's own kill, the leader pid (the group id) can be reaped and
recycled. Each parent group signal is withheld unless the supervisor is still an
unreaped running job of this shell or the group still has live members, so a
blind signal cannot reach an unrelated recycled group.

## Root Cause Not Confirmed

Planning (issue #1465) could not confirm the reported mechanism by which an
orphan from an earlier run reaped a *later* run's rig holder. No `scripts/` code
path has an orphan signal another run's holder; every `kill` targets a group the
signaling shell created. Two indirect paths fit (an orphaned `container` child
keeps `RuntimeCLIActive`'s host-wide `pgrep` positive; a stuck orphan holds a
state-root gate or DB lock), both ending in the later run's own cleanup
SIGKILLing its own holder. This change removes the orphans, which is sufficient
for every fitting path, without changing `freesided rig` or `daemonlock`.

## Refute-First Findings

A fresh-context adversarial reviewer tried to prove the change wrong (kill-path
review). Outcomes:

- **Confirmed and fixed: arming race.** The first version had the *parent* write
  the flag after forking the supervisor (fork, two `ps`, `printf`). A caller
  SIGKILLed in that window left the flag unwritten and the supervisor blocked
  forever on a never-exiting rig, the exact orphan being removed. Fixed by
  moving the write into the supervisor, which outlives the caller, so arming no
  longer depends on the caller surviving the window.
- **Confirmed and fixed: `set -e` fragility.** The drain guard's verdict was
  captured from a bare statement whose nonzero (group-gone) return would abort a
  caller running under `set -e`. Now captured inside an `if`.
- **Confirmed and fixed: flaky test timing.** The time-limit and
  no-signal-when-gone cushions were widened so a loaded runner cannot erode
  them.
- **Confirmed and fixed (automated review): shared-path deadman retarget.**
  The self-kill flag was a fixed per-session path (`rig-selfkill-pgid`). A
  recovery cleanup on the same session (after the caller was SIGKILLed)
  removed and rewrote it with its own group, so an earlier run's still-armed
  reservation read the recovery's group and killed that instead of its own
  orphan: the exact live-holder reaping #1465 targets, just displaced onto the
  recovery. Fixed by giving each invocation a unique `mktemp` record no other
  invocation can rewrite.
- **Confirmed and fixed (automated review): zombie leftover false positive.**
  The test's no-leftovers assertion checked tracked pids with `kill -0`, which
  a zombie still passes, so on a container whose PID 1 does not reap adopted
  children the holder-killed case reported a phantom leftover. Fixed with a
  state-aware `pid_alive` check that excludes `Z`, matching `group_present`.
- **Confirmed and fixed (automated review): reservation gave up before a slow
  arm.** The reservation waited a fixed 2s for the supervisor's arming write
  and read continued emptiness as "disarmed". If arming was delayed past 2s
  (process-table contention while the caller died at once), the reservation
  exited unarmed and the supervisor then ran the rig unbounded, recreating the
  orphan. Replaced the timeout heuristic with an explicit handshake: the
  supervisor always writes a decision (a pgid, or an explicit `disarmed`
  token), and the reservation waits for it bounded by the same wall-clock
  limit, so emptiness now means only "not yet decided". The former "pre-arm
  window untested" residual below is closed by a deterministic `arm-delay`
  regression case that injects a slow arm and fails on the pre-fix code.
- **Confirmed and fixed (automated review): unresolvable arming not failed
  closed.** The explicit handshake conflated two cases that both wrote the
  `disarmed` token: a group shared with the caller (a safe, deliberate
  no-signal) and a group that could not be resolved because the process table
  could not be read. Treating the unresolvable case as safe let the supervisor
  run the rig behind a deadman that could never fire, so a later caller death
  recreated the orphan. Split them: an unresolvable group now returns failure
  and the supervisor aborts before starting the rig, matching the fail-closed
  discipline `real_work_group_members` and the drain probe already follow.
  Covered by return-code assertions in `selfkill-flag` and an `arm-unresolvable`
  case asserting the rig never launches.
- **Confirmed and fixed (automated review): zombie caller delayed the deadman.**
  The reservation's caller-watch used a bare `kill -0`, which a zombie (a
  caller SIGKILLed but not yet reaped by a lazy parent) still answers, so the
  deadman waited out the wall-clock limit instead of firing promptly. This is
  the production instance of the round-1 zombie-blind `kill -0` class that was
  fixed only in the test's leftover check; the class sweep should have reached
  it then. Added `real_work_caller_gone`, which treats `Z` as gone but keeps an
  unreadable process table (while `kill -0` still succeeds) as alive, so a
  transient `ps` failure never fires the deadman against a live caller. Covered
  by a deterministic `caller-zombie` case that stages a durable zombie.
- **Allowed by decision: lone-live-leader skip.** `real_work_group_members`
  excludes the leader, so a supervisor leader that is alive, not a running job of
  this shell, and has no other members would be treated as drained. Unreachable
  on every real path: while the supervisor blocks in `wait` the reservation is a
  live non-leader member, and once the reservation ends the supervisor exits
  rather than lingering as a lone leader. Reaching it needs an external SIGSTOP
  of the leader specifically.
- **Allowed by decision: final-check pid reuse.** The test's no-leftovers check
  can false-positive if a recorded pid is recycled to an unrelated live process
  within the run. Same accepted residual as the drain's, at test scale.
- **Superseded: pre-arm window untested.** This was left untested on the
  reasoning that the window was closed structurally by the supervisor writing
  the flag. Automated review showed the reservation's fixed 2s wait reopened
  it under a slow arm (see the arm-delay entry above). It is now tested: the
  `arm-delay` case injects a slow arm via a test-only wrapper in the caller
  script and asserts the deadman still fires, failing on the pre-fix code.

## Rejected Options

- **Keep the drain note's "never exits on its own" rule and widen caller
  timeouts.** Leaves a TERM-immune orphan with no bound; the reported symptom
  is exactly such an orphan.
- **Reservation computes its own group with a self-pid trick.** No portable
  bash 3.2 method; the parent already knows the group id cleanly.
- **`kill 0` from the reservation to hit its own group.** Works only if the
  reservation is in a new group and still needs a same-group guard; the explicit
  parent-recorded pgid is both portable and unambiguous.

## Revisit When

- A production caller starts invoking `real_work_bounded_rig` inside `$( … )`
  or from a subshell whose death (not the script's) must end the group; then the
  `$$` caller pid is too coarse and needs the subshell's pid.
- Someone confirms a different root cause on the daemon side (the host-wide
  `pgrep` in `RuntimeCLIActive`, or the blocking gate lock in `acquireRigGate`);
  that is a new issue, not more work here.
- The reservation's short poll fires often enough on a supported platform that
  its fork rate matters; raise `real_work_reservation_poll`.
