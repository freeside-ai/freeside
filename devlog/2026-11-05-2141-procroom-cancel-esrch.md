# Proc Room Reports a Vanished Group as Done (#584)

## Decision

- **Chose to map `ESRCH` to `os.ErrProcessDone` in the proc room's own
  `Cancel` callback over moving `ProcRoom.Run` onto `procbound.Bind`.** The
  convergence is a separate unit (#584's non-goals): `procbound`'s
  `unboundSites` still exempts `internal/verify/procroom.go:Run`, and moving
  the room touches its option validation, reap, and that exemption in a Tier A
  verdict path, none of which this fix needs. The callback now matches
  `Bind`'s mapping exactly and returns every other kill error unchanged.
- **Extracted the callback as `cancelProcessGroup`** so a test can call it
  after the command is reaped, which makes the race deterministic. `Run`'s
  signature, `WaitDelay`, `Setpgid`, post-wait reap, and result
  classification are unchanged.

## Refute-First Result

The claim to disprove: mapping `ESRCH` to done lets a step that cancellation
killed read as passed.

- **Disproved by reading Go 1.26.10's `os/exec`.** `Cmd.Wait` takes
  `Process.Wait`'s result first and uses `watchCtx`'s error only when that
  result is nil, so a nonzero exit or a signal death stays an
  `*exec.ExitError`. `kill(-pgid)` returns `ESRCH` only when no member of the
  group exists (a darwin zombie gives `EPERM` instead, below), so the kill
  delivered nothing and the exit status the room reports is the one the step
  produced on its own. `watchCtx` drops only an
  `os.ErrProcessDone` result; the fix changes nothing about a successful kill,
  which still yields `ctx.Err()` (a room error, unchanged, and the zombie case
  the contract leaves out).
- **Disproved by a stress probe** (not committed): 400 `ProcRoom.Run` calls
  each of `true`, `sh -c 'exit 3'`, and `sleep 1` under random 0 to 4 ms
  timeouts. No `exit 3` or `sleep` run read as passed under either callback.
  The old callback turned 13 of 400 `true` runs into `ESRCH` room errors; the
  new one turned none.
- **Escaped descendant still fails the step** (from the same reading, not
  probed). When the leader exits 0 and a descendant that left the group with
  `setsid` holds the output pipe, the group kill returns `ESRCH`. Before the fix that read as a room error; now
  `WaitDelay` expires and `Run` reports `ErrWaitDelay` as exit code -1, a
  failed step. Neither reads as passed.
- **Confirmed, deferred: the darwin zombie window still fails closed.** On
  macOS, `kill(-pgid)` on a group whose only member is an unreaped zombie
  returns `EPERM`, not `ESRCH` (the pre-push reviewer's probe, rerun). So the
  fix covers only cancellation after the reap; a cancellation between the
  step's exit and its reap still reports a room error, never a pass. Mapping
  `EPERM` to done was rejected: `EPERM` also means a live member the daemon
  can't signal, which would then read as passed. Follow-up: #2692.
- **Allowed, out of scope:** a kill that lands on a recycled group ID after
  the leader is reaped is the post-wait PID-reuse question in #585, unchanged
  here.

Revisit when `ProcRoom.Run` moves onto `procbound.Bind`; the callback then
goes away.

Scope: `daemon/internal/verify`, `devlog/`.
