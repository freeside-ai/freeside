# Retry an Adapter Proof That Ran Out Its Budget

#3574 has the daemon retry a Claude adapter proof that failed only because
its 3-minute budget ended, and lifts a start's launch-coverage hold when a
later proof passes. This revises "Don't retry a timed-out adapter proof" in
`2026-11-17-2250-timeout-under-load-messages.md` (#3337). The work is Tier A
by path (`internal/daemoncompose/agent_selection.go`, `claude_driver.go`,
`agent_cutover.go`).

## Decisions

- **Retry, revising #3337.** That note chose a restart message over a
  retry. Its assumptions changed:
  - The overrun is routine, not rare. #2382's rerun on 2026-11-20 ran out
    the budget on both subscriptions (3m7.5s against 3m0s) under the exit
    run's own image builds and cache fills, and an unattended run can't
    restart.
  - The retry doesn't slow the start. The note weighed an in-pass retry
    that adds a budget per candidate to start conformance. This one runs
    from the conformance follower after the start.
  - The backoff is long enough for load to clear. The retries wait 5, 10,
    and 20 minutes (`followerBackoff`), each behind a reservation that
    waits for memory, in place of the short backoff the note rejected.
  - A restart is no longer the only way to resolve the item. The note's
    revisit trigger was "when something reruns conformance without a
    restart"; this change is that.
- **Retry only an overrun on every subscription.** Chose to retry when every
  proof that ran failed by its budget, over retrying any failure, because a
  proof that failed a check on its merits fails the same way again. A mixed
  result, one subscription over budget and another failing on its merits,
  is not retried: the narrow reading of "a merits failure is not retried".
  `proveAdapterOnCandidates` returns `adapterProofOverranError` for the
  first case and nil for the rest, as before. The deadline stays in the
  error's text, not its chain, per #3337.
- **Rerun the adapter proof alone, not the suite.** Chose
  `runAdapterProofRetry` over a full follower pass because the suite passed
  and stays published, and rerunning it adds the load that caused the
  overrun. It runs as a restart blocker under its own proof-pass
  reservation, serialized with every other pass by `passMu`.
- **Keep an overrun out of the scheduled pass's in-pass retry (#3653).**
  #3653 reruns a scheduled re-prove, suite and all, once after 5 minutes
  when every cause ends in an `*exec.ExitError`. An overrun keeps the
  runtime's error in its chain, and a budget kill can make that an exit, so
  `reprove` checks `adapterProofOverran` first and leaves an overrun to the
  adapter-only retry, over letting the suite rerun and add the load that
  ran the proof out of time. `reprove` keeps its error-only signature: the
  loop recognizes the typed `adapterProofOverranError` with
  `adapterProofOverran`'s walk, not `errors.As`, which would take one
  overran member of a join for the whole, and `proveScheduled` clears the
  image's failure for an overrun instead of recording one. A pass the
  budget refuses for good now reaches the loop as its error, not nil, so
  the loop can tell a pass that ran no adapter proof from one that passed.
  When the in-pass retry runs, its adapter proof, the newer record,
  settles the round.
- **Bound the retries per series, three.** A series ends when a proof
  passes or fails on its merits, when another pass's adapter proof
  overtakes a waiting retry (an epoch counter), or when a retry fails any
  other way (a drain, a pause, a reservation refused for good). Each
  scheduled round settles its last adapter proof as a fresh series, since
  that proof is the adapter's record. After three overruns the daemon logs
  that admission stays held until the next scheduled pass or a restart.
- **Recheck only a launch-coverage hold, and run the full writer check.**
  `agentSelectionGate.recheck` lifts a start's writer-role failure only when
  it is an `engine.AgentAdmissionRefusal` at
  `AdmissionCheckAgentLaunchCoverage` and `CheckWriterRoles`, the start's own
  check, now passes every role. Chose that over trusting the new adapter
  record, so the gate re-gates against current store state and fails
  closed. Any other start-time failure stays fixed for the daemon's run.
  The recheck runs after any adapter proof that may have passed, a retry's
  or a follower or scheduled pass's, so a start whose proof failed on its
  merits also lifts once a later proof passes.

## Refute-First Findings

- **Disproved: the recheck could never pass while the gate is closed.**
  `CheckWriterRoles` calls `AgentSelection.CheckRole`, which never consults
  `Gate`; only `engine/admission.go`'s attempt admission does.
- **Disproved: a non-coverage hold could lift.** The only
  `AgentAdmissionRefusal` in a `CheckRole` error is the one
  `resolveAgentAdmission` returns, so its check names the step that
  refused. A prompt mismatch, a credential failure, or a plain error is not
  rechecked, and a misread would still need `CheckWriterRoles` to pass.
- **Disproved: the recheck reads a stale lineup.** `agents` is written only
  during composition, before the follower that calls the recheck exists.
- **Disproved: the retry's reservation sweep could release a live pass.**
  `DaemonMemory.ListLeft` skips reservations this process holds, and
  `passMu` keeps the retry from overlapping a follower or scheduled pass.
- **Disproved: the start now fails on an overrun.** It never did:
  `proveAdapterOnCandidates` returned nil for every failure before this
  change, so the start, the follower, and the scheduled pass treated an
  overrun as a pass. They still do, and now also schedule the retry.
- **Fixed: a retry could start after `Close`.** `Close` canceled without
  `mu`, so a retry scheduled between a context check and `wg.Add` could race
  `wg.Wait`. `Close` now cancels under `mu`, which also covers `request`.
- **Fixed: a spent series starved a later pass's overrun.** A follower pass
  that overran after an earlier series used its three retries got none,
  since only the scheduled round reset the count. The pass now starts its
  own series.
- **Fixed: an overrun joined with a failed reservation release read as an
  overrun.** `adapterProofOverran` now counts a join only when every member
  overran, so the release failure fails the retry rather than hiding in a
  log.
- **Disproved: the in-pass retry marks an overrun as a runtime failure.**
  `reprove` wraps the retry's error in `reproveRuntimeFailure` only when
  it is not an overrun, so the doctor never counts an overrun at all.
- **Disproved: a pass refused for good now gets an in-pass retry.** Its
  chain ends in `ErrLaunchNeverFits` or `ErrRuntimeOverheadUnknown`, not
  an exit, so `runtimeCommandFailure` is false and `reprove` returns it.
- **Allowed: an overrun joined with an exit reruns the suite.** A join is
  an overrun only when every member is, so it is a failure, and #3653's
  in-pass retry applies to it as to any runtime command failure.
- **Allowed: a proof no subscription could run ends the series.** A retry
  would meet the same subscriptions, which don't depend on the image, so the
  series ends there as before #3574.
- **Allowed: a stale item if a retry fires during start's activation.** If
  the first retry, 5 minutes after composition, lifts the gate between
  `ActivateAgentSelection` reading the failure and writing its item, the
  item stays open with the gate open until the next start. Activation reads
  the gate's current failure, which closes the window for a retry that
  finished first.

Revisit when the adapter conformance record gains a failure reason, so the
gate could tell an overrun from its record, or when memory reservations
(#3206) change how a proof waits for load.
