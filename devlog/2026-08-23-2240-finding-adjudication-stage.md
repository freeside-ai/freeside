# Finding Adjudication Stage (#840)

Issue #840 implements the adjudication design recorded in
`2026-08-11-1504-review-finding-adjudication.md`. The implementation keeps
route authority in the immutable artifact and reconstructs human decisions
through a store-owned boundary, rather than trusting item payloads or
caller-supplied command structs.

## Decisions

- **Causal command history uses an ordering invariant, not exact version
  arithmetic.** The store reconstructs the current gated item and every
  immutable command by ID, requires one terminal command, requires its action
  to have been offered, and requires the resolved successor to have a strictly
  later item version with identical item, head, and artifact bindings. Exact
  predecessor arithmetic was rejected because legal delivery updates can
  advance a terminal item again.
- **Interim dispute attention remains a parked state.** A persisted
  `review_dispute` at the adjudication item identity is recognized before the
  finding-command decoder runs. Treating every item at that identity as a
  command-bearing `finding_adjudication` item was rejected because critical
  ceilings and dispute routes deliberately create the existing dispute type.
- **Disposition time comes from durable authority.** Automatic dispositions
  use the artifact creation time; a human-selected disposition uses the
  item's durable decision time. Sampling the workflow clock during replay was
  rejected because it changes immutable bytes after an after-commit crash.

## Refute-First Verification Findings

An independent trust-boundary lens confirmed and the implementation fixed
three reachable defects before commit. Automated review then confirmed and
the implementation fixed two more members of the same authorization class:

- The fetched publication checkout is repository-only, so using its directory
  as the base tree made candidate-deleted paths appear absent on both sides.
  Adjudication now retains separate base and candidate worktrees from their
  bound SHAs.
- Re-sampling `now` while replaying a committed disposition caused an
  immutable conflict. The record now uses the durable artifact or item time,
  and an after-commit restart test advances the clock before replay.
- Replaying an artifact-backed dispute tried to decode the dispute item as a
  finding-adjudication command item. The engine now recognizes the durable
  dispute first, and a two-pass regression keeps it parked without error.
- A threshold-meeting persisted classification could reach the engine fast
  path even when inference rejected its lattice or producer-labeled note.
  Fast-path routing now requires successful reconstruction-time inference
  revalidation; an unavailable evaluator also leaves the finding as residue.
- Filesystem `Lstat` followed intermediate worktree symlinks, so a host path
  outside the bound tree could appear to exist. Path resolution now walks each
  component without following intermediate symlinks, matching Git's rule that
  a symlink blob has no descendants.

The lens rejected the remaining authorization hypotheses after tracing the
store and engine gates: later item-version advances preserve the causal
command; a stop command returns before route execution; payload-only
alternatives cannot grant a route; model residue fails closed before artifact
creation; and readiness consumes reconstructed, artifact-bound dispositions.

## Revisit When

Revisit the causal-history reconstruction if Signet adds another legal
terminal command for finding adjudication, permits terminal status changes
other than later-version delivery updates, or introduces a first-class command
identifier on the terminal item.
