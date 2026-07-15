---
run: manual
stage: store-snapshot-metadata
date: 2026-07-15
branch: feat/store-snapshot-meta
---

# Expose store snapshot metadata for command acceptance (issue #91)

Spine-role session, fiat-assigned; #91 was inserted into the Wave 1
contract chain (tracking issue #83) as part of the same planning
operation. Discovered by #65's first claim: the acceptance boundary
must check the API contract's `ClientCommand.expected_entity_version`
and return the exact original `CommandResult.revision` on idempotent
retries, but both values existed only as SQLite columns
(`entity_version`, `as_of_revision`) that no `ReadTx` method selected.
Declared path: `daemon/internal/store`.

## Decisions

**New Snapshot variants, legacy Gets delegate.** `GetAttentionItemSnapshot`
and `GetCommandSnapshot` return `(entity, Snapshot{EntityVersion,
AsOfRevision}, error)`; `GetAttentionItem`/`GetCommand` keep their
signatures and discard the metadata. Rejected: widening the existing Get
signatures (touches every caller for two call sites that need the
metadata) and a `Versioned[T]` wrapper (one more concept for the same
two int64s).

**No `PutCommand` change; the committed result is read back in-tx.**
`WriteTx.asOfRevision` is `current+1`, stamped into the row at insert,
and the single-connection, `_txlock=immediate` store makes that exactly
the revision the transaction commits as, so a `GetCommandSnapshot`
after `PutCommand` inside the same `Write` returns the committed
result on both the fresh-accept and retry paths (one code path for
#65). Rejected: returning the revision from `PutCommand` (a second way
to learn the same fact, and it still leaves the retry path needing the
read).

**Metadata is range-checked at reconstruction, fail-closed.** Item:
`entity_version >= 1 && as_of_revision >= 1`; command: `entity_version
== 1` (write-once row) `&& as_of_revision >= 1`. STRICT tables enforce
type, not range, so these checks are the only rejection of a
schema-legal but store-impossible value (the trust-boundary re-gate
convention, AGENTS.md).

## Refute-first verification (returned-object trust boundary)

Two independent refuter lenses over the diff before commit:

- **Confirmed, fixed**: the exact retry path (byte-identical
  `PutCommand` replay + in-tx `GetCommandSnapshot`, then abandoning the
  `Write`) was untested; a future edit re-stamping `as_of_revision` on
  replay would have passed the suite. Now pinned by
  `TestCommandSnapshotReplayInsideWrite`, which also pins that an
  abandoned `Write` leaves the server revision unmoved (the #65 replay
  design depends on it).
- **Confirmed, fixed**: forged-metadata corpus only used zeros; negative
  values and command `entity_version = 0` added per the adversarial
  input-enumeration convention.
- **Rejected by verification**: an upper-bound check
  `as_of_revision <= server_state.revision` at reconstruction. Inside
  the accepting `Write`, the row is stamped `current+1` while
  `server_state.revision` is still `current`, so the bound would fail
  the exact in-tx read #65 needs; weakening it to `revision+1` encodes
  Write's internals into every read. The range checks plus
  store-only column writers are the accepted posture.
- **Verified, no change**: revision serialization (single pooled
  connection plus `BEGIN IMMEDIATE`) makes concurrent stamp collisions
  impossible; an errored `Write` callback has zero side effects;
  `NewEpoch` never touches `revision` or row metadata.

Revisit when: a read pool splits reader/writer connections (#30's
condition); the single-connection serialization argument above must be
re-verified for the same-tx read guarantee.

Follow-up: #65 consumes these reads at the signet acceptance boundary.
