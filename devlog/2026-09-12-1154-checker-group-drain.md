# check-agent-image.sh Needs the macOS Group-Kill Drain at Its Interrupt Site

`scripts/check-agent-image.sh` carries the same post-kill group drain that
`real_work_bounded_rig` gained in #1290: after SIGKILLing a runtime process
group it re-sends the group KILL until only the leader survives. This note
records why the checker turned out to be reachable, superseding the exclusion
in `devlog/2026-09-11-0910-cleanup-supervisor-drain.md` section "Why
`scripts/check-agent-image.sh` Is Excluded". Context: issue #1300.

## The Per-Site Determination

The race needs a group member forking at the instant the group KILL lands
(macOS misses it; the escapee survives). The checker runs each runtime call as
one process group holding the leader subshell, two `cat` relays that copy the
runtime's fifo output onto the checker's own stdout and stderr, and the runtime
CLI plus `head`. Only the relays hold the checker's caller-facing stdout/stderr;
the CLI and `head` write only to the fifos and the internal pipe. So the hang
shape (#1290: an orphan holds the caller's output open) needs an escaped relay.

- **Timeout site (watchdog KILL): not a hang.** The watchdog fires at least
  the bound (>= 1s) after the job started, long after the relays were forked, so
  no relay can be mid-fork then. A descendant that escapes at that instant is a
  runtime-CLI child holding only a fifo write end or the pipe to `head`; nothing
  waits on those after the KILL (the relays die, the main shell unlinks the
  fifos), so it is a stray process, not a hang.
- **Interrupt site (HUP/INT/TERM trap KILL): reachable.** The trap can fire at
  any moment, including the first millisecond of a job while the leader is
  forking a relay. An escaped relay holds the checker's stdout or stderr and
  blocks forever on a fifo that never gets a writer and is then unlinked. A
  caller capturing the checker's output through a pipe (`run_checker`'s `$(...)`,
  `freeside-project-image`'s `CombinedOutput`) then hangs. The window is ~1ms
  per call and macOS-only, so it is rare but real.

The frozen note excluded the checker because "its main shell reaps the runtime
leader while the watchdog kills it." Reaping the leader subshell does not close
a relay's inherited fd 1/fd 2, so it is not the deciding invariant. The drain
is added at both sites (matching the rig, and cheap at the timeout site) even
though only the interrupt site can hang.

## Rejected Option

- **Source `scripts/real-work-lifecycle.sh` to share the drain.** #1300's
  non-goals leave that file alone, the checker is a standalone operator script,
  and the lifecycle helper is copied into sessions. Kept `runtime_group_members`
  and `drain_runtime_group` as an aligned copy instead; the two should be kept
  in sync (noted at both sites).

## Revisit When

- The drain's `ps`/zombie filter behaves differently on a supported platform.
- The drain runs where the pid space can realistically cycle within its ~1s
  bound (extreme churn against a very small `pid_max`), making the accepted
  pid-reuse residual (the leader pid, reaped asynchronously) reachable.
- The relay/fifo structure of `bounded_runtime_process` changes such that a
  different group member comes to hold the checker's caller-facing descriptors.
