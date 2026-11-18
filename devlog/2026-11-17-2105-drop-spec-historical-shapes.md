# The Daemon No Longer Reads Historical Specification Shapes

The daemon kept readers for six stored specification shapes that nothing
writes any more, so a database from before each change would still resume.
This note records why they go now, the one reader that stays, and what that
leaves. It covers #3322, family 4 of #3266's inventory.

## Decisions

- **Remove the readers rather than keep them for old databases.** The owner
  decided on 2026-11-16 that prod history so far is disposable and prod
  starts on a fresh database (#3265). No database a current build opens
  holds any of these shapes (Evidence). Keeping them would keep a second
  output decoder, two submit replays, an intake fallback, a second input
  order, and two decision sets per validator for data nobody has.
- **Keep the reader for approval claims without a summary.** The seed
  fixture (`daemon/internal/seedfixture/runs.go`) writes a specification
  approval with one claim and a terminal with no `summary_digest`, so its
  readers are live: the `summaryDigest == nil` branch of
  `verifySpecificationApprovalClaims` (engine), and
  `authenticatesInitialApprovalClaims` and
  `blockedWaitTerminalSummaryMatches` (store). Removing them needs the seed
  fixture changed first, which may change what the app's seeded screens
  show; that is a separate unit.
- **Keep the research content-type bound and change only its error.**
  `DecodeResearchEvidence` is the only check on a stored envelope's content
  type, so deleting the bound would let an old oversized envelope read as a
  current one. Chose to fold it into the metadata check, returning
  `domain.ErrParentKeyMismatch`, over keeping `ErrResearchTooLarge`: the
  fetch refuses such a content type before it stores an envelope, so a
  stored one over the bound is bad metadata, not a research failure. The
  cost: such an envelope now fails as an operational error that
  reconciliation retries, not as a typed undeliverable-input failure. No
  instance holds one.
- **An old shape now fails loudly, never reads as the current one.**
  - A comment-keyed addressal fails the strict output decode; the run
    records an interpretation failure.
  - A campaign submission over a campaignless stored run fails as `stored
    specification run disagrees` (`ErrImmutableTransition`); a reserved run
    without the submission's campaign as `reserved specification run
    disagrees with submission` (`ErrParentKeyMismatch`).
  - A label-intake reservation without its first attempt fails in
    `startSpec` with the attempt lookup error.
  - The pre-#698 input order fails as a request its preceding transition
    does not authorize (`ErrParentKeyMismatch`).
  - An approval set without Discuss fails as an item not bound to its run
    (engine) and `ErrParentKeyMismatch` (store); a Stop-only revision
    failure set as a binding that disagrees.
- **Submissions with no campaign at all still work.** Only the replay of a
  campaign submission over a campaignless run goes. Making the campaign
  required is a different change.
- **A same-version backup from before the fresh database still restores,
  but its old rows fail when read.** Restore copies rows, not schema, so a
  row in a removed shape restores and then meets the refusals above.

## Evidence

- **Writers, confirmed in code.** No production code, seed fixture, or
  `freeside-signet-dev` writes any removed shape:
  - The pre-#920 prompt digest appears nowhere else in the repository.
  - Label-intake admission reserves the run with a campaign and writes its
    first attempt in the same step; manual submit, client task submission,
    and operator feedback all set a campaign. Campaigns came in with
    34cddeceb; the attempt-at-admission rule with 82091b293 (#1491).
  - The research continuation builds the input vector by role through
    `specificationInputs` (since 77161f65f, #698).
  - The engine writes approval items with Approve, Request Changes,
    Discuss, Stop, and revision-failure items with Discuss, Stop; the seed
    fixture writes the same four-action approval. `freeside-signet-dev`
    seeds items with no specification run behind them, which the engine
    validators never read.
  - The fetch refuses a content type over 8 KiB before it stores an
    envelope.
- **Instance counts (2026-11-17, counts only, from a backup-API copy of each
  database, deleted after use).** Counted: specification requests and runs
  without a campaign, any row carrying the pre-#920 prompt digest, approval
  items without Discuss, Stop-only revision-failure items, stored research
  envelopes over the bound, and specification requests created before
  #698 closed. Both spellings, `specification` and the pre-rename
  `elaboration`, were counted.
  - **Dev instance:** the branch's `check-database` passes; 0 of every
    shape, and no stored blobs.
  - **Kept roots at schema 120 or later (9):** 0 of every shape. Two pass
    the branch's `check-database`; seven fail it on kinds this unit doesn't
    touch (check proof, schedule occurrence, artifact, attention item),
    identically under `main`'s build (4481f907a).
  - **Kept roots below the migration baseline (27, schema 22 to 104):** no
    current build opens them (#3302's note). Six hold 9 approval items
    without Discuss in all, and one of those six also holds 7 campaignless
    specification requests and runs. None holds the other shapes.
- **Prod.** Not read by an agent for this unit. #3265 records prod started
  on a fresh database on 2026-11-16.

## Refute-First Record

- **A remaining decode error is still a research request failure, so the
  removed engine branch is live (disproved by reading).** Every return in
  `DecodeResearchEvidence` wraps a strict-decode error,
  `ErrImmutableTransition`, `ErrParentKeyMismatch`, or an unwrapped
  `FetchRequest.validate` error; none wraps `ErrResearchURLRefused`,
  `ErrResearchTooLarge`, or `ErrResearchFetchFailed`.
- **The intake fallback still served a reachable path (disproved).** Once
  the engine refuses a campaignless reservation, the fallback could only
  build a spec the submit then refused.
- **A current writer still produces a removed shape (disproved by a
  fresh-context reviewer's search).** Every approval writer, the one
  revision-failure writer, every submit path, and every input-vector builder
  write the current shape; retries copy the stored vector.
- **An old shape now passes as current (disproved).** Each one reaches the
  refusal listed under Decisions.
- **The diff drops a kept test's rationale (confirmed, fixed).** Removing the
  input-order test also cut the doc comment of the test after it, and the
  retry request's comment still named a stored order that no longer varies.
- **The issue-subject refusal has no test (confirmed, accepted).** A
  reserved run without the submission's campaign now fails with
  `ErrParentKeyMismatch`. No test pinned the old replay either, and the
  manual-submit refusal beside it is pinned.

Related: `devlog/2026-11-16-2138-drop-pre-rename-vocabulary.md` (family 1).

Revisit when a database from before these changes needs to resume: it needs
a migration path first, and then these readers.
