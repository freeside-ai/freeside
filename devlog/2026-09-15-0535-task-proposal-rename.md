# Keep the effect-registry encoding literal while renaming the run_proposal vocabulary

Issue #1210 renamed the `run_proposal` attention type to `task_proposal` across
the daemon, API, and clients (plan revision 48 / #1206 had already renamed it in
the plan). This note records the one non-obvious decision the rename forced.

## Chose to keep `run_proposal` as the effect-registry encoding over a new encoding version

The rename carried `AttentionType`, the `getTaskProposalFacts` operation and its
`/task-proposal` path, the `TaskProposal*` schemas, and the
`task_proposal_revision` decision-payload field to the new vocabulary. It did
**not** rename the effect-registry wire encoding: the `EffectKind` value, the
`"kind":"run_proposal"` value, and the `"run_proposal"` JSON key of
`EffectProposal`'s canonical encoding all keep the literal `run_proposal`. The
Go identifiers still renamed (`EffectRunProposal` -> `EffectTaskProposal`, the
`RunProposal` field of `EffectProposal` -> `TaskProposal` with its
`json:"run_proposal"` tag retained), with an explanatory comment at the
`EffectKind` constant.

Chose the retained literal over a full rename because the canonical encoding is
hashed into the proposal content digest, and that digest binds approvals
(`effect_proposal_items.content_digest`, the item's `artifact_digests`, the
decision ledger) and addresses the proposal artifact. Renaming the literal would
change every existing digest, so it would require a new
`EffectProposalEncodingVersion`, a digest re-binding migration over persisted
proposals, and a table rebuild for migration 0041's `effect_kind` CHECK. That is
a separate, heavier unit; the owner can schedule it if the encoding literal is
worth changing. This was an agent planning decision, recorded so a later reader
does not read `EffectTaskProposal = "run_proposal"` as a bug.

## Data migration re-gate (refute-first)

Migration 0074 rewrites persisted `attention_items` rows (`item_type` column and
body `$.type`) from `run_proposal` to `task_proposal` and advances the sync
revision. The store re-gates attention rows on read, so this is a
returned-object trust boundary. The refute-first checks:

- **Decision surface stays valid.** The decision-surface preimage
  (`decision_surface.go`) hashes item id, epoch, subject, requested decisions,
  PR head SHA, and presented digests, not the item type. A migration test
  confirms the stored surface digest is byte-identical before and after the
  rewrite, so the re-gate still matches.
- **Effect-proposal binding stays valid.** The migration leaves
  `effect_proposal_instances` untouched (retained encoding), so the item's
  bound `content_digest` still resolves to a reconstructable instance. The test
  reconstructs the instance and checks the binding digest.
- **`json_set` re-serialization preserves escaped bytes.** SQLite's `json_set`
  re-emits the whole body. A seeded row carrying `& < >` in a body field decodes
  back byte-for-byte after the rewrite, matching migration 0035's precedent.

## Client-cache token migration preserves the on-disk ledger

The rename also touched the app's persisted disk cache (`CacheStore.swift`),
which keeps its `format` at 5. Two persisted tokens change under the rename:
the attention snapshot's `type` value (`run_proposal` -> `task_proposal`) and
the `run_proposal_revision` decision-payload key (-> `task_proposal_revision`).
An in-place upgrade reading a pre-rename format-5 cache hits both. `AttentionType`
decodes strictly, so a cached `run_proposal` snapshot throws and drops the whole
cache including the retryable command ledger; the generated payload decoder
instead ignores the unknown `run_proposal_revision` key, so a committed
start_with_changes command reloads without its revision and fails its verbatim
retry.

Extended the existing pre-decode preprocessing (renamed
`migratingLegacyCommandKinds` -> `migratingLegacyEncodings`) to translate both
tokens in place rather than bumping the cache format. A format bump would not
help: the format fallback path still decodes the full `CacheFile` first, so the
strict snapshot decode would throw before any format-based ledger salvage ran.
The translations are structural and location-anchored (never a blind string
swap), matching the file's established idiom; the `run_proposal` value is unique
to `AttentionType` across the schema.

## Revisit when

The effect registry gains a second kind, or an `EffectProposalEncodingVersion`
bump happens for another reason; fold the `run_proposal` -> `task_proposal`
encoding rename into that digest re-binding rather than doing it standalone.

Source queue entry: `devlog/2026-09-07-0921-task-vocabulary.md`.
