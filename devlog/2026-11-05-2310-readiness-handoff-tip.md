# The Ready Card Names The Tip Its Handoff Checked (#1735)

Tier A by path: the change edits `daemon/internal/engine/production_publication.go`
and `production_handoff.go`. It only projects the sealed handoff check
(#2644, PR #2660) onto the readiness detail; it grants nothing and changes no
gate.

## Decision

- **Chose `handoff_tip` and `handoff_merge` that omit when empty over
  rendering null.** Every ready item stored before this change lacks both
  keys, and the store compares a stored detail's bytes with its re-encoding
  (`daemon/internal/store/entities.go`). An explicit null would make every
  such row fail that comparison. This follows `CheckProof.MergeSHA`'s
  precedent against the golden explicit-null convention. `Validate` accepts
  an empty tip for the same reason; `NewReadinessDetail` requires one, so
  only stored rows lack it.
- **Chose a legacy-detail branch in `compatibleTerminalItem` over
  backfilling.** The detail is creation-immutable, so a stored item cannot be
  upgraded in place. Recovery compares a stored detail without a tip as if
  it named the rebuilt tip and merge, then keeps the stored shape as the
  transition baseline, as the existing legacy-nil branches do. Without it
  every ready item stored before the change strands its publication with
  "ready item disagrees with finalized publication".
- **Chose the evidence base as the tip when no handoff check was sealed.** A
  publication intent committed before PR #2660 has none, and
  `reconcileHandoff` already recovers it with the evidence base as its tip.
  The card names the same tip the handoff relied on.
- **Chose to have a re-entered cycle name its own base and no merge, over
  reading the sealed check.** The cycle keeps the original publication's ID,
  so the sealed check under it names the old head. A tip-only detail has no
  cross-check that would catch that mismatch, so reading it would put a
  wrong tip on the card silently.
- **Chose to fail a published candidate whose sealed check is moved and
  unproved.** The publication gate refuses such a candidate, so one reaching
  the ready item means a broken invariant, not a state to render.
- **Rejected projecting the detail inside `currentReadinessVerdict`.** The
  verdict is evaluated before the handoff runs, and the recovery path at
  `production_publication.go` reads only the verdict. Keeping the target and
  states beside the verdict and projecting the detail once the tip is known
  keeps one evaluation behind both, without threading the tip through every
  caller of the review re-gate.

## Refute-First Findings

Self-review and a fresh-context reviewer tried to prove the change wrong
before commit.

- **Confirmed by self-review, fixed: the finalized-recovery path built its
  expected item from an unprojected detail.** `reconcileTask` hands the re-gate's
  readiness to `loadReadyPublicationOutcome`, which rebuilds the ready item.
  The plan named only `completePublishedTask` and `completeReentryTask`; the
  recovery path now projects the handoff too. A zero detail would have
  failed item validation, so it failed closed rather than silently.
- **Disproved: the rebuilt tip can drift from the stored one.** A sealed tip
  is never re-resolved and its merge is fixed once proved, both before
  publication. A legacy intent yields the evidence base on both the handoff
  and the readiness side, and a re-entry never reads the sealed check.
- **Disproved: aliasing in the legacy compatibility branch.** It copies the
  detail into a new pointer, and `cloneReadinessDetail` deep-copies the
  merge.
- **Disproved: byte breaks on legacy rows.** Both fields omit when empty; the
  store test strips both keys and the row decodes and lists.
- **Disproved: the Go, OpenAPI, and Swift rules disagree.** All three apply
  the same tip and merge rules, including distinct commits.
- **Allowed: recovery of a legacy row is covered by a unit test only.** No
  integration test restarts a ready item stored without a tip; the
  compatibility branch's unit test covers the comparison.

## Revisit When

A ready item can be created before the publication gate accepts a proved
handoff check, or `HandoffCheck` stops binding the evidence base to the
admission base.
