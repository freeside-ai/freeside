# Handoff Stop Evidence: Verification Reason and a Stop Row

Issue #2658 names the cause when a handoff at a moved tip stops on the tip
itself (#2644, `2026-11-05-1530-handoff-check-implementation.md`). Three
stops come from the tip rather than the head: the rebuild gate refuses the
merge, the merged tree cannot be materialized for verification, or no image
serves the tip while image upkeep has nothing pending. Before this change
each blocked on the base-advanced reason with the authorizing checkpoint's
evidence, so the item never said why. The work is Tier A: it decides what a
publication block shows and how recovery rebuilds it.

## Decisions

- **Block on the handoff-verification reason, not base-advanced.** Chose
  `PublicationBlockHandoffVerification` over keeping
  `PublicationBlockBaseAdvanced`. The base-advanced reason's own doc comment
  reserves it for a handoff with nothing to merge (the head already at the
  tip, or a tip rewritten away), and those two keep it. A first publication
  already pairs the shared verification-failure reason with one daemon
  evidence artifact that names a refused rebuild clause, so the handoff
  follows that shape. Both reasons map to the same block facts, so
  observation does not change. Keeping base-advanced would have needed a
  handoff-only branch inside the base-advanced recovery case, which rebuilds
  the authorizing checkpoint's evidence for several reasons at once.
- **Name the cause in evidence only.** One daemon-written evidence artifact
  per stop names the cause, the head, the tip, and the merge. A rebuild
  refusal's first line is the first publication's,
  `Project image rebuild refused: <clause>.`, from one shared function. A
  per-clause reason text or a new block member would be a contract change.
- **A write-once stop row, not a reportless checkpoint.** The handoff's
  re-entry checkpoint cannot carry this evidence: `loadReentryCheckpoint`
  reads the outcome back from the verifier's own report, and a stop has
  none. Chose a new engine-private inbox row (`production_handoff_stop`,
  keyed beside the handoff checkpoint) over loosening that read, which is
  the Tier A check that keeps a checkpoint's outcome from being trusted as
  decoded. The row is written before the item. A later pass reads it before
  verifying, so a restart blocks on the first-recorded evidence and never
  verifies the merge again; recovery of a handoff-verification block reads
  it before the failed-checkpoint path. The one exception is a base branch
  rewritten between the row and the item: the next pass blocks as a
  rewritten tip before it reads the row, and recovery of that block is
  consistent. No migration: the inbox table exists.
- **The unservable-tip text claims only what every path checked.** Three
  errors carry the image-incompatible class to the stop: no recorded image
  serves the tip with nothing pending in upkeep, a rebuilt image an earlier
  pass bound that no longer serves the merge, and a bound row that
  contradicts its image. The evidence says no image this run may verify
  with serves the tip and quotes the refusal, without asserting what upkeep
  holds, because the text is written once and outlives the pass.
- **Rejected: taking the evidence from the stored item at recovery.** The
  retired re-entry path does that because the cycle that produced its
  evidence no longer runs. Here the handoff still runs, and a row lets the
  forward path and recovery derive the same evidence from one record instead
  of trusting the item a crash may not have written.

## Findings

- **A rebuild refusal at a moved tip needs a gate that changed after the
  first verification.** When the tip keeps the manifests the admitted image
  baked, the merge's manifests are the head's and its lockfile delta is the
  head's own, which the first verification already passed. A tip that
  changes the manifests, a recipe, or an unsupported input takes the
  unservable-tip path instead. So the refusal is reached across a daemon
  upgrade that tightens the gate, not by tree contents, and the integration
  harness cannot drive it. Its account is tested directly; the stop path it
  takes is the one the integration tests drive for the other two causes.

## Refute-First Findings

A fresh-context reviewer tried to prove the change wrong before commit.

- **Confirmed and fixed:** the first unservable-tip text asserted that
  upkeep had nothing pending on paths that never read upkeep (the decision
  above).
- **Allowed:** a base rewrite between the stop row and the item ends on the
  rewritten-tip block instead of the recorded stop (the exception above).
- **Allowed:** tree-cause evidence quotes the verifier's error with its
  internal wrapper prefixes, as the re-entry reasons already do; the quote is
  bounded, and rewording internal errors is outside this unit.
- **Disproved by a check:** the refactored rebuild-refusal evidence and
  re-entry tree reason are byte-identical; a handoff cannot hold both a
  checkpoint and a stop row; a retry cannot conflict with the write-once row
  (artifacts and row commit together, `createdAt` is the task row's);
  recovery's recipe set is a superset of the forward path's; the
  upkeep-pending error does not carry the image-incompatible class.

Revisit when the block contract gains a per-cause reason, which would let
the item's reason name the cause and retire the stop row's evidence role.
