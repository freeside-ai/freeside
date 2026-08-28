# Enforce Review Finding Locations Against the Reviewed Diff

Work unit: #967 (`kind:fix`, `lane:spine`). Wires the #679 `diffscope`
overlap validator into routed review ingestion so a reviewer finding is
admitted only against the exact reviewed base-to-head diff. The issue
body is the authoritative contract; this note records the load-bearing
decisions and the refute-first outcome.

## Decision: Gate the Routed Path Only, Before Any Persistence

The overlap check lives in `reconcileReviewGate`
(`engine/production_publication.go`), immediately after the existing
`run_id` contradiction check and before `NewReviewRecord`,
`reconcileRemediationReview`, and the persisting `store.Write`. A
non-overlapping finding is rejected before any review record, finding,
disposition, or remediation intent exists, so the crash/retry invariant
holds structurally: the derivation persists nothing, and every write is
downstream of a passed gate.

Scope is the routed source (`w.reviewSource`) only. The shadow-review
path is observation-only: it persists to a separate record and drives an
attention item, never adjudication or remediation, so it cannot
authorize autonomous work and is outside #967's harm model. Native
review-level comments (legitimately nil `Location`) ingest on a
different path and never reach this gate.

## Key Decisions and Rejected Options

- **Checkout source: reuse the retained review workspace.** The gate
  runs `git diff` against `reviewWorkspace` (= `req.Workspace`), the
  candidate checkout materialized at `HeadSHA` that the adjudication path
  already passes to `remediationCandidatePatch`; `RetainWorktree` copies
  the full object DB, so both commits resolve. Rejected: a dedicated
  fresh materialization per pass, as unnecessary work duplicating the
  retained workspace.
- **Diff flags: `-U0 --no-renames`, no `--binary`.** `diffscope.Parse`
  consumes zero-context text hunks (it rejects context lines and has no
  new-side ranges for a binary patch), so `-U0` is mandatory and
  `--binary` is wrong here. `--no-renames` is load-bearing: `diffscope`
  keys strictly on the new-side path, so under git's default rename
  detection a candidate that deletes a file and adds a similar one is
  re-keyed as a rename under the new path and the deleted path disappears
  from the diff. A whole-file `(0,0)` finding on that candidate-deleted
  file (the §7/#855 representation) would then fail closed in the *wrong*
  direction, burning a legitimate review and raising a false
  human-recovery item. `--no-renames` renders every touched path as its
  own delete/add pair, so both endpoints stay resolvable; it never
  fabricates a change on an untouched path, so the fail-closed rejection
  of genuinely off-diff findings is preserved. The original plan left
  rename detection on (mirroring `remediationCandidatePatch`, whose
  `--binary` patch feeds `git apply` and is rename-insensitive); the
  refute-first pass caught the divergence.
- **Header-only changes: seed touched paths from `--name-status`.**
  `-U0` alone is not enough. git emits only `diff --git` and metadata
  (no `---`/`+++` header, no hunk) for a file-mode change, a
  binary-content change, or an empty-file add/delete, so those touched
  paths never enter `diffscope`'s index. A candidate-deleted *empty*
  file is the reachable trap: its whole-file `(0,0)` finding (still the
  §7/#855 representation) would fail closed in the *wrong* direction,
  the same failure `--no-renames` guards for a non-empty deletion. A
  companion `git diff --name-status -z` pass over the same pair seeds
  every touched path through `diffscope.MergeNameStatusZ`, so a
  whole-file finding on a header-only change resolves while genuinely
  untouched paths stay absent; a concrete line finding on such a path
  still fails (it carries no new-side range). This is not the
  `diffscope.Parse` redesign the non-goals exclude: `Parse` is
  untouched, and the merge is an additive touched-path seed the engine
  drives.
- **Error classification: reviewer contradiction vs engine fault.** Only
  `Overlaps == false` is a `ReviewFailureContradiction`. A failure to
  produce or parse our own deterministic diff is an engine fault returned
  on the pending/retry path: blaming the reviewer for our own diff
  failure would burn a legitimate review.
- **No new domain sentinel.** The non-overlap contradiction wraps the
  existing `domain.ErrParentKeyMismatch` (as the sibling `run_id` and
  base/head checks do), keeping the change inside
  `daemon/internal/engine` with no shared-contract edit.

## Changed Assumption: #855 Merged Mid-Stage, No Replan

The plan grounded at `main` @ `ea83df8c` treated `merges-after: #855` as
open and named a #855 `FindingLocation`/`Overlaps` change as its
highest-severity replan trigger. #855 merged via #972 before
implementation and changed neither: deleted-file findings are
represented as the whole-file location `(0,0)`, which
`diffscope.Overlaps` already accepts on a touched (including deleted)
path. So the gate accepts #855's representation with no adaptation, the
replan trigger did not fire, and the merge gate is now satisfied.

## Refute-First (Returned-Object Trust Boundary)

Ward findings are external returned objects, so an independent
fresh-context reviewer tried to prove the gate wrong before commit.

- **Confirmed and fixed:** the default rename-detection gap above. A
  delete-plus-similar-add candidate re-keyed the deleted path out of the
  diff, so a valid whole-file deleted-file finding was rejected as a
  contradiction. Fixed with `--no-renames` and a delete/add-rename engine
  test.
- **Confirmed and fixed:** a third integration fixture
  (`TestProductionNativeCleanPassNeverSatisfiesReadiness`) scripted its
  finding off the reviewed diff, so after the gate it silently exercised
  the contradiction path instead of its intended findings-coexist path
  while still passing. Moved onto the changed line, like the two sibling
  fixtures the change already updated.
- **Confirmed and fixed (Codex PR review):** `-U0` omits the
  `---`/`+++` headers for a header-only change, so a candidate-deleted
  *empty* file's whole-file finding was rejected as a contradiction, the
  per-file analogue of the base==head empty-diff case the pre-commit
  pass had only checked in whole. Fixed with the `--name-status` seed
  (Key Decisions above) and diffscope + engine tests over the
  empty-delete, mode, binary, and empty-add cases.
- **Checked and held:** the gate precedes every persistence; the base
  commit resolves in the retained round-1 workspace; an empty
  (base==head) diff parses to an empty scope, not an engine fault; path
  quoting/case divergence fails closed (exact match, never false-accept);
  the cumulative base->new-head diff keeps relocated findings resolvable
  across remediation rounds;
  `TestProductionFindingsDoNotSurviveInstructionAuthorityChange` keeps
  its off-diff finding because it is store-seeded and its polled round is
  clean, so it never reaches the gate.
- **Noted, not changed:** the crash test asserts write-ordering
  crash-safety, not gate existence (an overlapping finding passes with or
  without the gate); gate existence is proven by the off-diff rejection
  test, which fails if the gate is removed.

## Revisit When

- `diffscope`/`domain` deleted-path or whole-file handling changes, or a
  diff-side (old-side) location representation is introduced: re-verify
  the gate accepts every valid representation.
- A second routed review-ingest path is added: it needs the same gate.
