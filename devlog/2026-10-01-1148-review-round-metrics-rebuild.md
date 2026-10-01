# Review Round Metrics: Rebuilding the Previous Head

Work unit #1049, plan §7 "Review Drift". #1048 defined a round's metrics as
two pairs: base to head, and the previous recorded round's head to head. This
note covers how the engine counts the second pair when the previous head is
gone.

## The Previous Head Is Not in the Review Workspace

Each publication pass rebuilds the candidate as a fresh commit parented on the
base, and the review workspace is a copy of that checkout. After a remediation
the previous round's commit and tree exist nowhere on disk. The #1049 plan
assumed one `git diff` between the two heads; that works only when the head
did not change.

Two paths change the head between recorded rounds: a remediation, and an
operator-feedback successor (`return_to_agent`). Reevaluation, continuation,
retries, and configuration adoption keep it.

## Chose to Rebuild the Tree From the Stored Remediation Patch

The owner chose this on 2026-10-01 from the options on #1049.

The remediation input artifact already holds the base-to-previous-head diff
(`--binary --full-index`), sealed by digest and authenticated on every pass.
The engine applies it to the base tree in a scratch index, then counts from
the rebuilt tree to the new head.

- **Rejected: re-import the previous replay.** A remediation overwrites the
  task payload in place, and the export record holds digests only.
- **Rejected: store the previous tree or counts at round N for round N+1.**
  It needs new persisted state, and the patch already carries the content.
- **Rejected: keep only the cumulative pair.** The stop rule reads only that
  pair, but the stored row requires both, so this is a contract change behind
  the serialized contract chain.

## Chose to Verify the Rebuilt Tree, Not Trust the Patch

The patch is a returned object: bytes read back from the artifact store. The
rebuilt tree must equal the tree the verification checkpoint recorded for the
previous head (`loadRemediationSourceTree`), and the remediation request must
name the previous review record's head and this record's base. Any mismatch
refuses the count. The round pair is labelled with the previous record's head
commit, which is what the store's binding check requires, while the count runs
from its tree.

New objects go to a scratch object directory with the workspace's objects as
an alternate. The review workspace is evidence for the review source and is
never written.

## Chose a Gap Only When a Retry Cannot Help

Metrics are never backfilled (#1048), so a round recorded without them stays
without them. A deterministic refusal (no remediation input, a mismatch, a
patch that does not apply) records the round with no row. A cancelled context
or an environmental failure (a path or SQLite error) fails the pass before
the record is written, and the pass retries. The first draft turned every
failure into a gap; the refute-first pass showed a transient read fault would
then cost the round its metrics for good.

Rejected: failing the pass on every computation error. A deterministic
failure would then block review forever for a count the review does not need.

## Left Operator-Feedback Rounds as a Gap

A round after `return_to_agent` has no remediation input, so it is recorded
without metrics and the growth rule skips it and the next round. The feedback
input holds the same kind of patch, but which checkpoint records that head's
tree was not proven. Follow-up: #1669.

## Refute-First Findings

An independent reviewer tried to break the gate write before it was committed
(`docs/agent-workflow.md` §refute-first).

- **Confirmed and fixed:** transient failures became permanent gaps (above).
- **Declined, then fixed in review:** a work directory containing `:` breaks
  the alternate object path. The decline held that invocation IDs cannot
  produce one, but the operator-set publication work directory can, and every
  round after a remediation would then be a gap, so the growth stop could
  never fire. The path is now passed as one C-quoted entry.
- **Declined:** a gap carries no recorded reason. The workflow has no log
  sink, and a refusal is not a review failure.
- **Disproved:** the engine and the store choosing different previous records;
  the write failing the record transaction after a successor or reevaluation;
  a stale remediation binding or another base yielding a count; the patch
  writing outside the scratch index.

## Revisit When

- A third path changes the candidate head between recorded rounds: it needs
  its own source for the previous tree, or its rounds are gaps.
- The review workspace starts retaining earlier heads: a direct diff then
  replaces the rebuild.
- Gaps need diagnosing in production: record the refusal reason somewhere an
  operator can read it.
