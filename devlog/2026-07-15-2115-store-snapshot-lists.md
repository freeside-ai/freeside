---
run: manual
stage: store-snapshot-lists
date: 2026-07-15
branch: feat/store-snapshot-lists
---

# Expose transactional snapshot lists for signet sync (issue #98)

Spine-role session, fiat-assigned; #98 sits in the Wave 1 contract
chain (tracking issue #83) after the merged store snapshot metadata
unit (#91, PR #93) and before the signet sync unit (#66), which stays
blocked on this merge. Discovered by #66: `/sync/bootstrap` must serve
every synchronized collection plus `ServerState` from one SQLite read
transaction, but `ReadTx` had only single-entity Gets. Declared path:
`daemon/internal/store`.

## Decisions

**One scan function per entity, shared by Get and List.** Acceptance 2
("collection reads reconstruct through the same gates as the
single-entity read") is enforced by construction, not by parallel code:
each synchronized aggregate funnels scan → decode → column cross-check
→ metadata range-check (→ evidence re-gate for items) through one
function taking a `scanner` (the shared surface of `sql.Row` and
`sql.Rows`), and the List methods receive it as a method expression.
Rejected: duplicating the gate sequence per List (the exact drift the
acceptance criterion exists to prevent).

**The Get widening extends the #91 range checks to runs,
conversations, and deliveries.** Their SELECTs now include
`entity_version, as_of_revision`, so those Gets inherit the
`>= 1` fail-closed checks items and commands already ran. This is a
read-side strengthening only; no persisted row shape changed.

**`Snapshotted[T]`, one generic pair type.** The #91 note rejected a
`Versioned[T]` wrapper because Go's multiple returns already pair
entity and snapshot on a single Get; that rejection does not carry to
slice elements, where a pairing type is forced. One generic type used
four times (the package already uses generics: `decode[T]`) beats four
bespoke structs. Rejected: parallel slices (unindexable, easy to
misalign) and an iterator (collections are local-daemon scale;
materialized slices are simpler, noted on the methods).

**Lists are all-or-nothing.** One forged or corrupt row fails the
whole enumeration with the same errors the Get path uses, never a
silent skip: a bootstrap that omits state would present a client a
coherent-looking but wrong world. Documented on the List methods.

**Deterministic order is ascending primary key, BINARY collation.**
`ORDER BY id` for runs, conversations, and attention items;
`ORDER BY item_id, device_id, channel, attempt` for deliveries; spelled
in constant SQL (no runtime assembly) and documented on the methods, so
bootstrap output and tests never depend on SQLite row order.

**No upper-bound revision check, carried over from #91.** The #91 note
rejected `as_of_revision <= server_state.revision` at reconstruction
because an in-tx read inside `Write` legitimately sees `current+1`;
the List methods are reachable from a `WriteTx` (it embeds `ReadTx`),
so the same posture holds. The pure-`Read` bootstrap test asserts the
upper bound instead, where it is legitimate.

## Refute-first verification (returned-object trust boundary)

Two independent refuter lenses (trust boundary; correctness and test
strength) over the diff before handoff:

- **Confirmed, fixed**: a forged row failed a list with no way to tell
  which row; the enumeration loop now wraps each row's error with its
  1-based position (stable under the documented key order, the only
  identity a whole-table read can name).
- **Confirmed, fixed**: the enumeration helper returned the partially
  built slice alongside a non-nil `rows.Err()`; it now returns nil
  results on any error, so no caller can use a truncated list.
- **Confirmed, fixed**: the concurrent-write isolation test's write
  touched only the devices table, so its torn-state branch on the item
  list could never fire; the write now advances the very item the list
  returns, and the test also asserts the advance is visible after the
  read commits.
- **Rejected by verification**: gate divergence between Get and List
  (the method-expression construction makes it structurally
  impossible; every SELECT column list compared identical), evidence
  re-gate bypass on the list path, both directions of the nullable
  conversation_id cross-check, NULL/affinity/empty-key/unicode
  forgeries (STRICT tables plus domain validators), a corrupt row
  misclassified as not-found, ordering instability (no COLLATE clause
  exists in the migrations, so TEXT keys order by BINARY memcmp), and
  deadlock/flake in the isolation test (`-race` clean; the read never
  waits on the writer).
- **Accepted by decision**: a wholly self-consistent forged row
  (columns, body, and metadata all agree) reconstructs on both paths;
  the store authenticates consistency, not provenance, per its
  existing trust model. And the absent `as_of_revision` upper bound
  (#91's rejected check) means a forged huge revision passes both
  paths' `>= 1` gate; the bootstrap surface (#66) should assert
  per-row `as_of_revision <= ServerState.Revision` inside its one
  Read, where the bound legitimately holds, as the permanent bootstrap
  test here does.

Revisit when: a read pool splits reader/writer connections (#30's
condition): the single-connection serialization argument behind the
concurrent-write isolation test and the same-tx read guarantee must be
re-verified. Revisit when collections outgrow local-daemon scale: the
materialized whole-table lists and the wire pagination question move
to #66's successor.

Follow-up: #66 consumes these reads to build `/sync/bootstrap`.
