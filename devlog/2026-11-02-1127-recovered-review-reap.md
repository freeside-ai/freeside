# Reaping a Recovered Review

A started review that hits a contradiction keeps its usage pool slot until the
ward proves its containers gone, and the engine never inspects it again, so
only startup recovery reaped it. Once an operator recovered the contradiction,
the next round on a limit-1 pool waited for a restart (#2472, a known gap in
`2026-11-01-2206-review-slot-lifetime.md`).

## The Gate Aborts the One Recovered Review

When the routed review gate finds the latest contradiction recovered, it asks
the source to abort that invocation (`AbortRecoveredReview`) before the next
round reaches the pool gate. The ward runs the authenticated abort task
cancellation uses and marks the outcome ready, which ends the slot under the
existing release rule.

- **Chose a targeted abort over rerunning startup recovery.**
  `CodexReviewRecovery.Reconcile` treats every started intent as orphaned by a
  restart, so a runtime pass would kill other runs' live reviews.
- **Chose an optional adapter over a `ReviewSource` method.** It is asserted
  the way `CancelReview` is, so `exec.ReviewSource` and its contract stay
  unchanged.
- **The gate repeats the call every pass** while the recovered contradiction is
  the latest failure. Once the outcome is ready the call returns before reading
  anything else, so a repeat costs one journal read and no runtime call.

## A Recovered Row Is Not Raised Again

The fresh-context review found that the first version wedged a run it should
free. A rejected request (rewritten, undecodable, or legacy) is reaped and
marked ready when its contradiction is recorded, but the abort's request
preflight raised the rejection again as a contradiction, so every pass after
recovery failed loud and the recovery never took effect. A rejected outcome did
the same through `rejectPersistedOutcome`, which returns a contradiction even
after a clean reap.

Chose to read the ready marker first, reap a rejected outcome directly and
report success, and abort past a rejected request, because reaping needs only
the durable intent. The recovered row is the contradiction the operator already
judged; the abort owes only the proof that the containers are gone. A request
read that fails for any other reason is transient, not a contradiction.

## Failures Keep the Slot

A transient abort failure becomes a retryable hold and retries on a later pass.
Any other failure fails loud, as startup recovery does. That includes a foreign
or unprovable object, and an outcome that is not ready and whose completion
evidence fails verification. Before this change a recovered run on a pool with
room advanced while those containers might still run; now it stops loud.
Chose that over letting a run proceed past containers the ward can't prove
gone, the same rule a restart already applies.

## Refuted

The review traced these and found them safe, so they needn't be asked again:
an unrecovered contradiction is never aborted (the call follows the recovery
check); only this run's recovered invocation is reaped, never the next round or
another run's review; an abort racing an in-process launch or `CancelReview`
serializes on the run gate after the outcome fence, as cancellation does; and
both composed production sources (Codex and Claude) are the concrete ward
source, so neither lacks the adapter.

Revisit when a review source lands that doesn't write the Codex review journal,
or when contradiction recovery gains a path that reruns the same invocation.
