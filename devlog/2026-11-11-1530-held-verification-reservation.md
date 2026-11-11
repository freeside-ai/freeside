# Re-Prove a Held Verification Reservation When Its Task Ends

Issue #2977, deferred from #2954
(`devlog/2026-11-09-2147-verification-release-proof.md`). Tier A: the change
adds a ward absence proof and touches `production_verification.go`.

The owner chose to re-prove, alert, and offer a command on 2026-11-11, over
two other options. One was accepting container absence without a joined
command for every capacity release. That would give up #2954's strict proof
on every pass and still not fix the cleanup-failure path. The other was an
operator command with no alert, which would leave a held reservation silent
until someone looked. Every automatic release keeps the strict proof. Only an
operator who confirms the old CLI is gone (`-command-gone`) gets the
container-only proof, and the daemon still proves every owned container
absent itself.

## Decisions

- **A reservation has ended when its run has no pending task.** The lane
  checks this at the end of `reconcile`, against the pending tasks that pass
  read. If any pending entry failed to decode, the pass counts nothing as
  ended, because the unreadable entry could belong to any run; it still
  concludes alerts whose reservation something else released, since that
  needs no pending-task view. Chose the run
  over the task because reservations are recorded per run, and one task's
  retried runs each hold their own.
- **The proof runs under the lane's `reconcileMu`.** Every
  `reserveVerification` caller (first-cycle, rebuild, re-entry, and handoff
  verification) is reached only through `reconcile`, so while the lock is
  held, no verification job is about to launch under an invocation the proof
  is checking. The operator command takes the same lock with `TryLock` and
  reports the lane busy rather than waiting behind a long pass.
- **Once per process, not a background sweep.** The issue ruled out a
  periodic retry. A re-proof that already failed fails again until something
  outside the daemon changes, and the operator command is the retry for that.
  A restart re-proves once more.
- **An alert concludes when its reservation is released, whatever released
  it.** This could be a later pass's proof, a confirmed task stop, or the
  operator command. The lane tracks alerted invocations in memory, loads open
  alerts on its first pass, and concludes any whose reservation is no longer
  open. An alert is never refiled for an invocation that already has one,
  open or concluded, so an acknowledged alert doesn't come back after a
  restart.
- **Ward names the container in a cleanup failure.** Ward's cleanup errors
  did not reliably say which container failed, and the alert must name it.
  The proof now wraps each record's failure with its container id, or with
  the owner label when there is no id. The unjoined failure wraps a new
  sentinel, `ErrVerificationCommandUnjoined`, with the same message, so the
  alert can pick the right instructions. Neither change alters what the
  strict proof accepts.
- **The macOS slot hold (#3000) is out of scope.** Today a quarantined slot
  holds the reservation only for its own pass, so the end-of-task proof
  checks containers only. If #3000 lands, its slot proof joins this proof and
  the command.

## Refute-First Results

- **The end-of-task proof races a launching job: disproved.** A call graph
  walk from `reserveVerification` over the engine package reaches only
  `reconcile` as a root, and the proof runs inside `reconcile` under
  `reconcileMu`. The command needs the same lock.
- **The lane releases a pending task's reservation: disproved.** Both the
  sweep and the command skip any run with a pending task, and a decode
  failure stops the sweep from counting anything as ended. Tests cover a
  pending run through a real lane pass and an unreadable pending entry. The
  fresh-context review found the first test called the sweep with a
  hand-built map, so dropping the line that records a pass's pending runs
  still passed; the test now runs the pass, and fails with that line
  removed.
- **`-command-gone` releases with an owned container present: disproved.**
  The container-only proof skips only the joined-command check. It still
  refuses a live call, retries owned cleanup, and proves every owned container
  absent. Ward and engine tests both cover a present container.
- **The ward refactor changes `StopRun` or `ProveInvocationAbsent`:
  disproved.** A scratch differential test compared the old and new functions
  over 1,500 generated sets of ownership records and runtime states, in both
  modes: 3,000 comparisons, 1,743 of them failing proofs. It compared
  nil-ness, `ErrVerificationAbsenceUnproven`, the empty-id refusal, and the
  containers left behind. Every comparison matched. The scratch test was not
  committed.
- **A crash-interrupted command whose CLI still runs is released with
  `-command-gone`: allowed.** The daemon cannot see an orphaned CLI, which is
  why the flag is the operator's word and is logged. The daemon still checks
  container absence. The README tells the operator how to confirm the CLI is
  gone first.

Revisit when the ward runner can prove an orphaned CLI gone by itself; the
operator confirmation could then go.
