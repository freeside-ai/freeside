# The Daemon No Longer Reads the Old Publication Shapes

The engine and the publish ledger kept readers for stored shapes that no
current code writes: a task field, a checkpoint key, item wordings and IDs,
notice types, missing item fields, and two intent formats. This note records
why they go, the evidence that nothing writes them, and what stays. It covers
family 5 of #3266, split out as #3323.

## Decisions

- **Remove the readers rather than keep them for old databases.** Owner
  decision, 2026-11-16 (#3323): prod history so far is disposable and prod
  starts on a fresh database (#3265), so nothing needs to read these shapes.
  No database a current build opens holds one (Evidence).
- **An old shape now fails loudly, never reads as the current one.** Each
  removal ends in a refusal the daemon already raises:
  - A strict decode error, which quarantines the publication task, for a task
    carrying `remediation_noop` or `authorizing_task_key`.
  - `ErrParentKeyMismatch` for a task under a version 5 or 6 successor, an
    undeliverable notice typed `execution_failure`, and a task item with no
    origin row.
  - The existing binding-mismatch error for a review item with the Codex
    wording or a pre-#611 dispute.
  - "Disagrees with task" for a terminal item missing its readiness,
    readiness detail, handoff tip, or yield history, and
    `ErrImmutableTransition` for a delivery refusal without admitted facts.
  - "Unsupported format version" for a publication intent at format 1 or 2,
    and "payload version invalid" for an outbox row at a retired version.
  - Two shapes are simply no longer found: a version 1 verification
    checkpoint (the task verifies again), and an in-place re-entry's old
    exhaustion item (the cycle raises its own).
- **Close one silent acceptance.** Before this change a publication task
  under a version 5 or 6 successor with no producer decoded as an in-place
  re-entry and could run forward. `validateWithPublication` now refuses
  every such task, with a producer or without one
  (`TestProductionPublicationTaskUnderRetiredReentryIsRefused`).
- **Some refusals stop a whole pass, not one run; accepted on the fresh
  database.** A pending intent at a retired format or payload version fails
  the outbox listing, so the publication drain and task cancellation stop.
  A task item without an origin fails unattended admission's health check
  once any workflow closure exists, and an old undeliverable notice fails
  the operator-feedback pass. No database a current build opens holds any
  of these (Evidence), so the daemon-wide stop is a loud signal, not an
  outage risk. A per-row quarantine would be new mechanism for rows nobody
  has.
- **Refuse a task item without an origin rather than guess its owner.** No
  writer creates a task-subject item without its origin row, so the
  single-start guess goes with the ID parse, and workflow closure fails
  rather than repair such an item.
- **A same-version backup taken before the fresh database no longer
  restores to a working state.** Restore copies rows, not schema. Its old
  rows restore, then fail as above when read. No such backup is known to
  hold one; every database found with an old shape is below the migration
  baseline (120), so no current build opens it anyway.

## Kept

- **The unsealed committed intent branch in `reconcileHandoff`**
  (`engine/production_handoff.go`). Removing it was tried: a pending intent
  committed before handoff checks then published against the later tip (one
  ref, one PR, one merge in the integration harness), which the acceptance
  forbids. It stays; the deferral issue proposes replacing it with a hold.
- **The publish-block nil branch in `compatibleTerminalItem`.** It has a
  live writer: the shadow-review stop writes its trust block with no facts,
  and recovery rebuilds the definitive reason's facts. The unconditional
  display-name copy stays too; renames need it.
- **The engine's successor chain walk in `productionTaskForInvocation`.** It
  reads stored successor records for any invocation, not only the retired
  re-entry.
- **The version 5 and 6 successor records' own rules and readers** in
  `internal/domain/`, `internal/store/`, and
  `internal/publish/disposition_history.go`. `internal/domain/` is outside
  this unit's paths; the deferral issue carries them.
- **`domain.PublicationBlockReentryRetired`** stays a definitive block
  reason in `internal/domain/observation.go`, so signet still reads it as a
  base-advanced block while engine recovery no longer writes or accepts it.
  No writer produces it; `internal/domain/` is outside this unit's paths, so
  the deferral issue carries it.
- **A stored terminal item without display names still matches.** The
  unconditional display-name copy accepts it; the removed branch only reset
  a value that copy overwrites.
- **The missing `Subject.TaskID` fill and the missing-conclusion tolerance**
  in `compatibleTerminalItem` (optional-field non-goal), `request.Legacy`
  and `rootMarker.Legacy` (#3321), and the Tier B tamper tests.
- **`TestProductionReviewHardLimitItemIDPreservesLegacyAndSeparatesRecovery`
  and `TestProductionNonContradictionHardLimitConvergesOnLegacyItem`.** Both
  pin live behavior: today's hard-limit item ID is still
  `productionReviewItemID`, and the second covers a crash between writing
  that item and finishing the task.

## Evidence

- **Last writer of each shape**, by `git log -S` over `daemon/`:

  | Shape | Last written by |
  | --- | --- |
  | `remediation_noop` on a task | Only the carry-forward; legacy when added in 6f842565d (2026-08-24) |
  | Version 1 verification checkpoint | Writer removed in 6f842565d (2026-08-24) |
  | "Codex review" item reason | Reworded in e77114a10 (2026-10-21) |
  | In-place re-entry's old exhaustion ID | Changed in 68c9f822e (2026-10-07) |
  | Pre-#611 dispute at a review item ID | Changed in 574dd7163 (2026-08-08) |
  | Task under a version 5 or 6 successor | Store refuses to seal one since 567421d46 (2026-11-05) |
  | Undeliverable notice typed `execution_failure` | 2976c7d91 (2026-09-15) and d64274537 (2026-11-05) |
  | Task item without an origin row | Origins bound since 9a1733879 and 23d83c210 (2026-10-20) |
  | Terminal item without readiness, detail, yield history, or names | Always set by today's constructors; handoff tip required since 053cd4c6d (2026-11-05) |
  | Delivery refusal without admitted facts | #979; selection fields since #2598 |
  | Intent formats 1 and 2, outbox payload version 2 | Format 3 since 7f7662fd6 (2026-09-07); version 2 from 3119e7d86 |

  The seed fixture and `freeside-signet-dev` write ready and blocked items
  but queue no publication task, so recovery never compares them.
- **Instance counts (2026-11-17, branch build, counts only).** Each database
  was copied, counted, and the copy deleted. Counted: tasks with the removed
  fields or a version 5 or 6 successor; run-keyed checkpoints; publication
  intents at payload version or format 1 or 2, and pending ones without a
  handoff check; items with the Codex wording; undeliverable notices typed
  `execution_failure`; held-work items without an origin.
  - **Dev instance:** `check-database` passes; every count 0.
  - **Kept roots at schema 120 or later (9):** every count 0. Two pass
    `check-database`. Seven fail it on kinds this unit doesn't touch (check
    proof, schedule occurrence, artifact, attention item), identically under
    `main`'s build (4481f907a).
  - **Kept roots below the migration baseline (27, schema 22 to 104):** no
    current build opens them. Nonzero counts: 9 intents at format 1 or 2 in
    six databases, 14 run-keyed checkpoints in four, and 14 Codex-worded
    items in three. Those at schema 81 or below predate the origin table.
- **Prod.** Not read by an agent for this unit; #3265 records prod's fresh
  database.

## Refute-First Record

- **A version 5 or 6 task runs forward after the route goes (disproved by a
  check).** The refusal test above covers both versions, with and without a
  producer.
- **Removing the unsealed-intent branch publishes against the later tip
  (confirmed by experiment; branch kept).** See Kept.
- **A removed `compatibleTerminalItem` branch has a live writer (confirmed
  for the publish block, kept; disproved for the other five).** Reading the
  ready and blocked constructors and their callers found the shadow-review
  stop's nil facts; the engine and integration suites pass without the
  other five.
- **The intent format-marker gate lost its tamper tests (confirmed,
  fixed).** Two tests checked that a payload rewritten to format 1 can't
  claim a format its outbox marker doesn't. Format 1 now fails decode
  first, so both raise the marker to 4 over a format 3 payload and still
  reach the mismatch gate in `DecodeStoredIntent`.
- **Fresh-context review (no blocking finding).** It confirmed no live
  writer for any removed shape, that a version 5 or 6 task is refused before
  any in-place handling, that no current task loses behavior, and that
  formats 3 and 4 behave as before. Its notes:
  - Whole-pass refusals (confirmed; allowed by the decision above).
  - `PublicationBlockReentryRetired` left in domain (confirmed; kept, see
    Kept).
  - The terminal-item commit overstated the display-name case (confirmed,
    fixed in the commit message and this note).
  - A stale `ValidateIntentDispositionHistory` comment (confirmed, fixed).
  - The converted readiness test dropped its foreign-readiness check
    (confirmed, restored).
  - Closure's refusal of an origin-less item has no test of its own
    (declined: the refusal lives in `AttentionItemWorkflowEpisode`, which
    `TestTaskItemWithoutOriginIsRefused` covers, and asserting it again at
    closure is the several-layer shape the test rules forbid).
