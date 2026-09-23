# Publication Author Input Admission

## Decision

For #1527, use separate limits for the two trusted author inputs: 256 KiB
for the fully composed instruction snapshot, including framing, and 64 KiB
for the PR template. The owner raised only the instruction limit because
distinct host, repository, and nested rules may legitimately exceed 64 KiB.
Keep the review composer's separate 1 MiB delivery limit. Do not discard or
deduplicate similar instructions based on their text; the operator host file
must contain only the intended host rules because trusted-base discovery
already supplies repository rules.

Preflight reads the admitted base commit's objects and applies the same
source precedence and composition as runtime. Its new Git reads ignore
ambient Git overrides and replacement refs. This matters even after the
base identity check: a replacement ref can preserve the requested commit
name while substituting shorter content. Instruction sources remain regular
files, while a tracked PR-template symlink remains admissible because
runtime reads its stored blob text. Preflight never consults candidate or
working-tree edits.

Keep runtime rejection advisory: both author sites fall back independently,
and a refused closure proposal cannot create a closing reference. Persist
only a fixed validation cause, field name, size, and limit in private
checkpoints. An optional closure reason keeps old checkpoints readable.

## Refutation

- **Confirmed and fixed:** A local replacement ref made preflight accept a
  short substituted instruction blob while runtime read the oversized real
  blob. The regression test now requires preflight to read the real blob.
- **Confirmed and fixed:** Preflight rejected a tracked template symlink that
  runtime accepts as a blob. The regression test now requires the same
  admission decision.
- **Confirmed and fixed:** Preflight's 4 MiB tree-listing cap rejected large
  repositories within publication's 100,000-entry and 64 MiB listing limits.
  Preflight now uses the same listing budget; a real Git fixture above 4 MiB
  with small author inputs exercises admission.
- **Disproved by comparison:** Extracting path selection from runtime
  discovery did not change per-directory override precedence or ordering.
  The old and new decisions matched on 50,000 generated path sets.
- **Model budget remains distinct:** The current subscription CLI reports
  usage after inference but exposes no supported pre-call token count. Its
  existing 1 MiB serialized-field limit cannot establish that the complete
  author prompt fits the configured model. Do not describe field-bound
  success as a model-budget proof.

Revisit when the subscription runtime exposes a supported pre-call count and
model budget, or the owner adopts a separately justified conservative bound.
