# Give Production Publication Its Own Loop, Not Its Own Workers

Work unit: #425. Scope: `daemon/internal/engine`, `daemon/cmd/freesided`,
`daemon/internal/integration`, and `devlog/`.

## Decision

**The production publication lane advances on a second daemon loop, beside the
reconcile loop, rather than inside it.** `Engine.Reconcile` no longer touches
the lane at all; `Engine.ReconcileProductionPublications` is the lane's own
pass and `Engine.RunProductionPublications` its own loop, composed in
`newDaemon` next to `Engine.Run` on the same interval. That is the whole
mechanism: no worker pool, no per-task goroutines, no result buffering, no new
shared state.

The starvation this fixes was scheduling, not fault containment (#418's
subject). One `reconcileTask` clones the exact base, imports, runs a
containerized verification, and calls GitHub; for that whole span the single
reconcile pass returned nothing, so no unrelated run, invocation, or attention
item advanced.

**Every publication invariant is untouched, because the lane's code did not
change.** Per-task serialization is still the on-disk task lock plus the lane's
own `reconcileMu`; exact-base binding, retry pacing (`holdRetryAfter`),
effectively-once publication, and the quarantine and hold projections all run
exactly as before, just on a different goroutine.

## Why Not Per-Task Workers

The obvious alternative reads the issue's "per-task serialization" as a call
for a worker per task: `reconcile` scans and dispatches, workers run
`reconcileTask` under the task lock, and a later pass drains their outcomes.
Rejected on cost against benefit:

- **It buys parallelism nobody asked for.** The acceptance criterion is that
  *unrelated engine work* advances, not that two publications verify at once.
  Concurrent publications would contend for the same work directory, container
  runtime, and transport anyway.
- **It adds the failure modes the lane is built to avoid.** Result buffering
  and drain semantics move the lane's outcome accounting off the pass that
  produced it, and worker lifetime becomes a second shutdown contract to get
  right. A second loop inherits `Engine.Run`'s: cancel, and the in-flight
  boundary's context ends with it.

Consequence accepted: **one parked publication still delays the next
publication**, exactly as before this change. Revisit when a single daemon
routinely holds more than one pending production task and the queueing latency
is measured, not assumed; the worker-per-task design above is then the
starting point.

## The Hazard the Split Actually Introduced

A refute-first pass found one real defect, and it was **not** shared mutable
state (that audit came back clean): it was a **read-interleaving** in the
acceptance scan. `acceptProductionAttempt` reads the inbox terminal in one
transaction and then calls `hasQueuedCompletion`, which reads the outbox in
another. The publication lane commits its terminal and dispatches its task in
two transactions of its own. Once the lane runs on a separate loop, the scan can
read the inbox before the terminal commits and the outbox after the dispatch,
and the old code read exactly that pair as
`ErrImmutableTransition`: a fatal, bogus contradiction that would stop
`Engine.Run` at the **successful** end of a publication.

**Fixed by re-reading, not by re-ordering.** The terminal write
happens-before the dispatch, so an inbox read taken _after_ observing the
dispatch settles the question: a terminal that is still absent is the real
violation and stays loud. Merging the lane's two writes into one transaction
would have removed the intentional `afterTerminal` crash seam and the
recovery path `authenticatesTerminal` exists to serve, so the transaction
boundary stayed where it was.

Findings **rejected by verification**, recorded so they are not re-raised: no
data race on `productionPublicationWorkflow` fields (`holdRetryAfter` is only
reached under `reconcileMu`, `approvedRecipes` is a construction-time clone,
`observationPace` is mutex-guarded, and the acceptance path's cross-lane entry
points touch no mutable field); no `d.errs` deadlock or lost error (six senders,
one send each, buffer seven); no lane-ordering test weakening (the production
harness never composes the fake lane). Findings **accepted by decision**: the
hold-projection interleaving the pass raised cannot occur, because the
dispatch-path holds and the publication-lane holds belong to disjoint phases of
a run's life; and the daemon composition itself stays uncovered by tests, which
is a pre-existing gap shared with every other `newDaemon` loop, tracked
separately rather than grown into this unit.

## Verification

The regression is a scheduling proof, not a timing assertion: the fake
transport parks inside `FetchBase` on a channel the test closes, so a healthy
run never waits. `TestBlockedProductionPublicationLeavesTheReconcileLoopFree`
holds the lane there, submits an unrelated production run, and requires the
reconcile pass to dispatch it (`InvocationsStarted`, plus that invocation's own
admission record). `TestShutdownEndsAParkedProductionPublicationWithoutLosing
ItsTask` cancels the loop while it is parked and requires the loop to return
nil, no forge effects, the task row still pending and undispatched, and a fresh
engine over the same store to finish it.

`TestQueuedCompletionToleratesAConcurrentPublicationDispatch` covers the
interleaving above at the predicate, because the window sits between two reads
inside `acceptProductionAttempt` and no seam there could stage it: a dispatched
task with a committed terminal must be reported as owned, and a dispatched task
with no terminal must stay `ErrImmutableTransition`.

**Mutation-checked, not assumed.** Restoring the inline call in
`Engine.Reconcile` makes the first test hang to the 45 s test timeout rather
than fail an assertion, which is the bug's actual shape: the parked pass holds
`reconcileMu`, so the reconcile pass blocks behind it.

The ~90 integration call sites that asserted both lanes from one `Reconcile`
now go through a harness `reconcileLanes()` that makes one pass over each loop
and sums the results; assertions are unchanged. Two sites deliberately stayed
on the bare reconcile pass, because what they assert belongs to it: dispatching
an invocation, and the acceptance path's refusal when no publication lane is
composed.
