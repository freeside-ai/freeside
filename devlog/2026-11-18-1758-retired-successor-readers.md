# The Daemon No Longer Reads Retired Re-Entry Successors

The domain, store, publish, and engine packages kept the rules and readers
for publication successors at versions 5 and 6, the two retired re-entry
shapes, and the domain kept their block reason. The handoff path also kept a
branch that published a pending intent no handoff check sealed. This note
records why they go, the evidence that nothing writes them, and how each now
fails. It covers the Kept list of
`devlog/2026-11-17-2149-drop-old-publication-readers.md`, split out as
#3341.

## Decisions

- **Remove the readers rather than keep them for old databases.** Owner
  decisions: remove these readers (#3323, 2026-11-16), and add this unit
  to tracker #3267 (2026-11-18). Prod starts on a fresh database (#3265),
  and no database a current build opens holds one of these shapes
  (Evidence).
- **A retired successor fails as a decode error, everywhere.**
  `DecodePublicationSuccessor` refuses versions 5 and 6 with
  `ErrParentKeyMismatch`, since the version is no longer known. Every
  reader goes through it:
  - Sealing one was already refused (since 567421d46); the refusal now
    comes from validation, not a dedicated check.
  - `PublicationSuccessorChain` and the store's successor reads fail, so a
    run holding one resolves to no task: the engine's
    `productionTaskForInvocation` reads the whole chain before it returns
    any task, which also replaces the refusal #3340 added in
    `validateWithPublication`.
  - Disposition history no longer reads the chain at all. It took its
    lineage base from re-entry segments, which only these versions made;
    every current authority keeps the cycle's own base.
- **A backup holding a version 5 or 6 record fails checkpoint health.**
  `PublicationSuccessorBackupPayloadDigests` decodes every successor row,
  so the checkpoint is refused, not restored into a run that can't read
  itself. Restore copies rows, not schema, so the same holds for a
  same-version backup taken before the fresh database. No such backup is
  known to exist.
- **The retired block reason is no longer definitive.** With
  `PublicationBlockReentryRetired` gone, a stored item carrying its
  sentence maps to no hold reason. Signet skips the item, and the run's
  `publication_blocked` milestone then has no accepted item to explain it,
  so the run's read fails closed (the corpus case
  `publication_blocked/retired_reentry_reason`). The engine's own recovery
  already refused the reason as unknown after #3340.
- **Refuse a pending intent with no sealed handoff check, rather than
  recover it at the admitted base.** Handoff checks exist since e60f8856d
  (2026-11-05); every intent committed since sits behind one.
  `reconcileHandoff` recovered such an intent with a stand-in check at the
  admitted base, so it published without any check against the target's
  current tip. Deleting the branch outright was worse (#3340's experiment:
  it sealed the later tip and published against it). It now returns
  `ErrParentKeyMismatch` before sealing anything: no check, no merge, and
  nothing on the forge
  (`TestHandoffRefusesAnIntentCommittedBeforeHandoffChecks`). The refusal
  stops the pass, as the #3340 whole-pass refusals do; no database a
  current build opens holds such an intent.
- **`withPublishedHandoff` refuses a published intent with no check.** Its
  missing-check branch named the admitted base as the tip for a
  publication pushed before handoff checks. No current flow reaches it: the
  engine and integration suites pass with the branch turned into an error,
  and `TestHandoffRefusesAPublishedIntentWithoutASealedCheck` fails against
  the old fallback. It now returns the check it loaded, so recovery always
  composes its candidate from a sealed check.
- **The hand-back cycle never sees the refused state.** An in-place
  hand-back task has no producer and keys its handoff on
  `InPlaceHandoffID`. Only a fix task commits an intent under the
  publication's own ID, and that task has a producing invocation, so its
  handoff ID is the publication ID and its check is sealed before the
  intent commits. The integration hand-back tests pass with the refusal.

## Kept

These are the unit's non-goals; they stay, and the deferral issue carries
them:

- **`domain.PublicationReevaluationReentered`** and its case in
  `internal/signet/publication_reevaluation.go`.
- **The three `PublicationBlockBaseAdvanced*` reasons** (conflict,
  verification, round limit) and their recovery in
  `internal/engine/production_publication.go` and
  `internal/engine/publish_block_facts.go`.

## Evidence

- **Last writer of each shape**, by `git log -S` over `daemon/`:

  | Shape | Last written by |
  | --- | --- |
  | Version 5 successor (`unpublished_base_advance`) | Added in 540a25034 (2026-10-18); store refuses to seal one since 567421d46 (2026-11-05) |
  | Version 6 successor (`successor_base_advance`) | Added in 8296d64d1 (2026-10-21); same refusal since 567421d46 |
  | `PublicationBlockReentryRetired` item | Engine recovery from 567421d46 until #3340 (ceb413e12, 2026-11-18) |
  | Pending intent with no handoff check | Checks sealed before every commit since e60f8856d (2026-11-05) |

  Production code, `seedfixture/`, and `cmd/freeside-signet-dev/` hold no
  writer of any of them; only readers and tests remained.
- **Instance counts (2026-11-18, branch build, counts only).** Each
  database was copied with the SQLite backup API, counted, and the copy
  deleted. Counted: successor records at version 5 or 6, tasks under one,
  items carrying the retired reason's sentence, and pending intents
  without a handoff check.
  - **Dev instance:** `check-database` passes; every count 0.
  - **Kept roots at schema 120 or later (9):** every count 0.
  - **Kept roots below the migration baseline (27, schema 22 to 104):**
    every count 0 except one pending intent without a check in one
    database at schema 104. No current build opens a database that old.
  - The successor query found three version 1 records across the kept
    roots, so it reads real rows.
- **Prod.** Not read by an agent for this unit; #3265 records prod's fresh
  database.

## Refute-First Record

- **A version 5 or 6 run still resolves to a task (disproved).** The
  engine reads the run's whole successor chain before it returns a task,
  and `TestPublicationSuccessorRetiredVersionsAreRefused` pins the decode
  refusal for both versions.
- **Disposition history loses a live lineage (disproved).** Its re-entry
  segments came only from versions 5 and 6; the engine, integration, and
  publish suites pass on the cycle's own base.
- **Signet still authenticates a run blocked on the retired reason
  (disproved by a check).** The corpus case passes, and fails with the
  run authenticated when the reason is made definitive again.
- **The unsealed intent refusal reaches a live flow (disproved).** See the
  hand-back decision above; the engine and integration suites pass.
- **A first publication under a re-entry loses its route (disproved).**
  `MakesFirstPublication` now means a handoff conflict's first
  publication; the closure-before-first-publication test moved to a
  version 7 handoff conflict and passes.
- **Fresh-context review (no correctness or security defect).** It could
  not make a version 5 or 6 record decode, change a current successor's
  behavior, reach the handoff refusals, or authenticate the retired
  reason. Its notes:
  - The step 9 commit and this note said the old branch published against
    the later tip; it recovered at the admitted base (confirmed, fixed).
  - The `withPublishedHandoff` refusal had no test (confirmed, fixed with
    the test above).
  - Recovery's comments and `composePublicationCandidate`'s nil handoff
    still described a check-less intent (confirmed, fixed by returning the
    loaded check). The nil case stays for the closure-reference tests,
    which compose without one.
  - Stale re-entry comments: the memory-limit test and
    `ProspectiveMergeOnto` (confirmed, fixed).
    `ReadinessInvalidationBaseAdvanced` in `domain/attention_item.go` and
    `DescendsFromBase` in `publish/transport_head.go` (declined: a
    base_advanced readiness re-entry still has live handling in
    `engine/production_reentry.go`). The fake forge's comment is outside
    this unit's paths.
  - Disposition history no longer reads the whole successor chain, so a
    root candidate's history load no longer fails on a bad successor
    elsewhere in the run (allowed: no current state reaches it, since no
    successor exists while a root's intent is pending, and the engine's
    task lookup still reads the chain).
