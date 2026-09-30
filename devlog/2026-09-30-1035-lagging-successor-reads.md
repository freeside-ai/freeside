# Tolerate Lagging Successor PR Reads Before Raising a Repair Card

Work unit #1544. A successor publication after `return_to_agent` raised a
high-priority `publish_blocked` card telling the operator to repair GitHub
state, and the daemon's own retry cleared it 47 seconds later. Nothing
external was wrong. This note records a relaxed check on fields GitHub
returns, which is on the mandatory-note list.

## Chose a Pending Error Absorbed by the Engine's Paced Retry

Two successor reads can lag the branch for a moment after a push: the
head-branch PR listing coming back empty, and the listing still showing the
predecessor PR at the predecessor head. Both now return
`publish.ErrSuccessorObservationPending` (causes `ErrSuccessorPRNotListed`
and `ErrSuccessorPRHeadLagging`) instead of `ErrPublicationConflict`. The
engine classifies the class as retryable, so an attempt within the tolerance
records the `publication_environment` hold and waits `holdRetryInterval`
without writing a card.

- **Rejected: sleeping or polling inside the publish call.** It would hold the
  task lock and the reconcile pass for GitHub's lag, and it duplicates the
  pacing the engine already owns.
- **Rejected: treating every successor conflict as transient.** More than one
  PR, a foreign marker, a closed PR, another PR number, a wrong repository,
  base, or head ref, and a head that is neither the predecessor's nor the
  candidate's stay immediate conflicts. So do the branch-ref read after the
  push and the PR update response. Only the two shapes GitHub's own lag can
  produce are pending.
- **The stale-head case is narrow by construction.** `convergeSuccessorPR`
  runs only after the branch ref was read at the candidate head, and
  `validateSuccessorPR` already admits only the predecessor or candidate
  head. The pending branch additionally requires the predecessor head
  explicitly, so any other mismatch still fails closed as a conflict.
- **No effect happens while pending.** An empty listing before the push stops
  the attempt before the push. After the push, pending returns before any PR
  edit. The plan's rule that a missing or changed resource blocks the effect
  still holds; only the operator card waits.

## Chose Three Tolerated Attempts, Counted in Memory

The fourth consecutive pending attempt for one task raises the card with
`external_conflict`, about 90 seconds in at the default 30-second interval.
Its reason says what the daemon saw (no PR listed, or the PR not at the
pushed head) rather than claiming a conflict with the committed identity.
Past the tolerance each paced lagging attempt keeps raising the card, and a
later success supersedes it as before. A transient failure while the card is
open still records `publication_environment` for that attempt, as it already
did under any conflict card.

- **Only success or a non-retryable failure clears the count.** Rejected:
  clearing on any non-pending outcome. The refute-first review showed that a
  transient failure (a 5xx, a dropped push) at least once every four attempts
  would then postpone the card indefinitely for a PR that is really missing.
- **The count is process state.** A restart starts the window again. Rejected:
  persisting it; the cost is a later card for a PR that is really missing,
  and the effect stays blocked the whole time.
- **A real conflict after a lag is still carded at once,** because it takes the
  conflict path directly, whatever the count.
- **Cost accepted:** a PR that is really missing (the continuation `-missing`
  scenario) now raises its card after the window instead of at once.

## Refute-First Findings

- **Disproved: a foreign or hostile state classified as pending.** The
  stale-head branch needs `validateSuccessorPR` to pass first. Any other
  marker, number, state, coordinate, or head still conflicts, and the unit
  test asserts none of them reads as pending.
- **Allowed: fork PRs filtered out of the listing read as pending.** The
  listing drops a fork PR that shares the branch name, so the card arrives
  about 90 seconds later with the "no PR listed" reason. Nothing is pushed or
  created in that time.
- **Disproved: pruning keyed differently from the count.** Queue rows are
  enqueued under `task.intentKey()`, the key `holdRetryAfter` already prunes.
- **Confirmed and fixed: a transient failure reset the count.** See above.

## Revisit When

- A false repair card still appears in an exit run. The trigger here is an
  inference, since the daemon does not log which check failed; the next
  suspects are the branch-ref read after the push and the PR update response.
- Real GitHub lag regularly exceeds about 90 seconds, or the hold retry
  interval changes enough that three attempts no longer span a plausible lag.
