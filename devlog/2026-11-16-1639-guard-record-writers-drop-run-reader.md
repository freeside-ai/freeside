# Static Revision Guard for Three Record Writers; Dead Run Reader Removed

Records why #3260 replaced four runtime revision tests with a named list in
the meta-test, and why removing `ListOpenAttentionItemsForRun` keeps
coverage of the two Tier A gates its tests touched. The unit is Tier A
because part of it changes tests of those gates.

## Decisions

- **A static check guards the three synchronized `Record*` writers,** over
  keeping four tests that ran each writer and read the revision.
  `Store.Write` raises the revision once on every commit, and only `Write`
  and the migration runner build a `*WriteTx`, so a writer that lives on
  `*WriteTx` and not on `*InternalTx` can't commit without a bump.
  `TestInternalTxCannotWriteSynchronizedState` now names
  `RecordUsagePool`, `RecordAuthIdentity`, and `RecordClientEnrollment`,
  fails if any appears on `*InternalTx` (the pre-#2487 shape), and fails if
  any leaves `*WriteTx`, so a rename can't leave the list stale.
- **The list is by name, not by convention,** because `Record*` methods sit
  on both handles by design (`RecordInbox`, `RecordDeviceCredential`, and
  many more are intentional internal writers). A new synchronized `Record*`
  writer is guarded only once someone adds it. Discovering them needs a
  naming or marker change across the store; #3260 left that out of scope.
- **The run-scope tamper tests moved to `ListOpenAttentionItemRecordsForRun`**
  with every assertion unchanged. They test the column-versus-body guard in
  `listOpenAttentionItemsForRun`, which runs before either scan, and the
  hand-back seal (`publication_successor.go`, Tier A) relies on that guard
  through the live method. With the guard disabled, every corruption case
  but one fails through the live method, the same set that failed through
  the deleted one; `body_retargeted` is also rejected by row
  reconstruction, so it passed with the guard off before the move too.
  One fixture lost its purpose: the stale-recipe row in the isolation test
  showed that another run's stale-policy row couldn't block the snapshot
  scan. The history scan applies no evidence policy, so that row now tests
  nothing; it stays because the contract limited these tests to the method
  and name change, and the malformed row still tests isolation.
- **The `"list open run"` row was dropped, not moved,** from the two
  finding-adjudication re-gate tests. The live method reads through
  `scanAttentionItemHistory`, which skips `gateFindingAdjudicationItem` by
  design, so the row has no live reader to point at. The gate stays covered
  by the three other readers in each test, which share
  `scanAttentionItemSnapshot` with the deleted method: with the read-side
  gate disabled, all six of those subtests fail.

The owner planned the unit (#3260 and its plan comment); the agent
implemented it unchanged.

Revisit when a store writer gains a marker that lets the meta-test find
synchronized `Record*` writers without a list.
