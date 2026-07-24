# Mint Audit Repository Identity

Issue: #261. Follow-up to #259.

## Decision

Chose GitHub's positive, immutable numeric repository ID as the mint audit's
canonical repository identity, alongside the human-readable `owner/name`.
The ID already gates minting through the reviewed trust profile and is the
durable identity across rename, transfer, deletion, and name reuse; the path
remains display metadata.

Chose an explicit `0` legacy-unknown sentinel for rows written before this
contract over backfilling from current GitHub state. Historical rows prove only
the display path that was recorded at mint time. A present-day repository at
that path may be a renamed or entirely different object, so assigning its ID
would manufacture audit history. Migration preserves those rows with `0`,
while `RecordMintAudit` rejects non-positive repository IDs for every new
write.

Rejected persisting the field only at the store layer. The production path must
carry the same canonical ID through `publish.MintRecord`, `StoreRecorder`, and
`store.MintAudit`; otherwise an adapter projection can erase it while direct
store tests still pass.

## Refute-First Verification

The migration lens constructs a database at schema version 10, inserts a
pre-ID row, advances to the current schema, and requires that the row remain
readable with repository ID `0`.

The write-boundary lens rejects zero and negative IDs before SQLite. The
round-trip and production-recorder lenses require a positive ID to survive
both the typed store path and a complete mint through `StoreRecorder`.

The identity-fidelity lens records one canonical ID under two paths and then a
different canonical ID under the original path. It requires the rename to
retain identity and the reused name to remain distinguishable, refuting both
path-as-identity and current-state backfill.

Revisit when historical mint rows gain an external reporting surface: unknown
must stay explicit and must never be rewritten from current GitHub state.
