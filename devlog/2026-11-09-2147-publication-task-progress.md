# A Publication Task's Progress Is a Named Enum

#2856 names how far a forward-route publication task got before the
current pass (the continuation route returns before the facts are read). `productionPublicationProgress` lists four
states, `readPublicationProgress` reads the three durable facts that
decide them, and `reconcileTask` switches on progress or asks
`hasCheckpoint()` where it used to branch on the loose booleans `found`,
`readyExists`, and `finalizedIntent`. Behavior is unchanged apart from
the one moved read below. This is link 2 of the publication-states chain
(#2761's plan comment); link 1 is
`2026-11-08-1600-publication-task-routes.md`.

## Decision

- **Four states from three facts, first match wins:** unverified (no
  checkpoint), verified (a checkpoint, no ready item, no finalized
  intent), intent finalized (a checkpoint and a finalized intent, no
  ready item), and ready (a checkpoint and a ready item). A ready item
  without a checkpoint is no state; the mapping returns `false` and the
  read step turns that into today's error. That `false` is the only live
  path to the error, not a second check beside an explicit one.
- **Kept the committed outcome and the review state out.** The outcome is
  read by `loadPublicationOutcome`, which needs a composed candidate, so
  it can't be read up front. The review state already has
  `productionReviewState` and a switch with no `default`. Rejected: the
  issue's first draft, which listed five facts.
- **Moved the finalized-intent read up into the read step,** ahead of the
  blocked-item read in `recoverDefinitiveBlockedTask` and the
  review-configuration adoption read. Rejected: leaving it in place and
  finishing progress after the adoption read, which would split the read
  step in two for no behavioral gain. The move is safe because both
  reads it now precedes are read-only on every path that lets the pass
  continue, each read is its own transaction, and the pass writes nothing
  between the old and new positions (Refute-First Findings).
- **Each switch on progress has no `default`,** so `exhaustive` makes a
  new state name its arm. The verification switch's ready arm returns a
  plain internal error naming the state, as #2855's route switches do,
  because the ready recovery arm always returns.
- **Gave the retired re-entry caller a progress value too.**
  `retireReentryTask` (`production_handoff.go`) also calls
  `recoverDefinitiveBlockedTask`, which the plan missed. It reads no ready
  item or intent, so it passes unverified or verified by whether its
  checkpoint exists. Blocked-item recovery asks progress only whether a
  checkpoint exists and whether it is ready, so this matches the old
  `found, false` exactly. Rejected: keeping the booleans for that caller,
  which the issue's contract rules out.
- **No recording fake for the read order.** `w.store` is a concrete
  `*store.Store` and the engine has no read-recording seam; adding one
  would be a test-only abstraction for one comparison. The read-order
  comparison below carries the evidence instead.

## Refute-First Findings

Tier A by path (`production_publication.go`, `production_handoff.go`,
and the new `production_publication_progress.go`, which joins the Tier A
list). Each claim below tried to show the change wrong.

- **"Some fact combination now takes a different branch."** Disproved by
  check: a scratch test transcribed the old boolean decisions and ran the
  new code's decisions through the real `productionPublicationProgressOf`,
  `hasCheckpoint`, and switches, over all 32 combinations of checkpoint
  found, ready item, finalized intent, import account match, and pending
  intent. Every decision agreed: both errors, whether the intent is read,
  the two facts blocked-item recovery sees, recovery entry, the ready
  outcome, the recipe-revoked hold, and verification.
- **"The finalized-intent read moved past a write."** Disproved by
  reading. Store reads and writes from the checkpoint read to the recipe
  gate, in call order:
  - Before: checkpoint (inbox), ready item, blocked item, adoption
    transition, finalized intent (outbox, only with a checkpoint), then
    the recovery arm.
  - After: checkpoint, ready item, finalized intent (only with a
    checkpoint), blocked item, adoption transition, then the recovery
    arm.

  The blocked-item read returns with nothing written when the item is
  missing or under a publication-hold decision; every other path in
  `recoverDefinitiveBlockedTask` either errors or writes and returns an
  outcome, ending the pass. `reviewConfigurationAdoption` is one read
  transaction. So on every path that reaches the old position, nothing
  is written between the two positions. A concurrent dispatch landing in
  between looks the same as one landing just after the old read, which
  could already happen.
- **"The move changes what a failing pass does."** Confirmed and allowed:
  a pass that a definitive block ends, or that fails reading the
  adoption, now reads the outbox once more, and an outbox read failure
  now fails the pass before blocked-item recovery. Both are read-only and
  a failed read retries the pass.
- **"An error moved or changed text."** Disproved by reading: the two
  errors keep their text, and they can't reorder, because the
  ready-without-checkpoint error needs no checkpoint and the import
  check needs one. The import check still runs before the intent read.
- **"Progress is read after verification writes a checkpoint."**
  Disproved by reading: the verification switch is the last use of
  progress in `reconcileTask`.

## Revisit When

- Anything between the checkpoint read and the old intent position starts
  writing on a path that continues. Then the intent read has to move back.
- #2857 shares candidate composition between recovery and forward, or
  #2858 extracts the checkout and verification prefix; the recovery switch
  may then split by state.
