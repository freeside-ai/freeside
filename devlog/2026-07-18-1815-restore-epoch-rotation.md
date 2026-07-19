# Rotate the sync epoch during a real checkpoint restore (#165)

Work unit for #165 (P1, Wave 1 audit): make a database restore issue a
fresh `sync_epoch` through a real restore operation, not a bare epoch
hook. Companion to the already-merged #162 (client-side epoch-scoped
validation).

## Decisions

- **Chose an in-process ATTACH + table-copy restore over a file swap +
  startup detection.** A file-level restore (copy the checkpoint over
  the DB, detect a restored DB on `Open`, then rotate) leaves a window
  where the daemon serves restored data under the old epoch before
  rotation, and needs a "this DB was restored" signal that does not
  exist (the exact gap the audit named: `seedEpoch` only fills an empty
  epoch). The chosen `Store.Restore` pins one connection
  (`MaxOpenConns(1)`), copies every data table from the attached
  checkpoint, and rotates the epoch in the **same transaction**. The
  first instant any client can read restored data is post-commit, when
  the epoch is already fresh. Rotation is one method, one transaction:
  it cannot be forgotten or omitted, which is what the issue demands
  ("not a direct epoch hook that can be omitted by the real restore").

- **Revision legitimately rolls back to the checkpoint.** A restore is a
  rollback, so `revision` and every `entity_version` regress under the
  new epoch. Revisions compare only within an epoch, so the lower
  post-restore revision is never ambiguous against a client's higher
  pre-restore cursor; the epoch change forces a full discard first. This
  differs from `NewEpoch` (which never moved revision), and the signet
  restore test was rewritten to assert the rollback, not revision
  stability.

- **Suspended FK enforcement (`PRAGMA foreign_keys = OFF`) for the copy
  rather than `defer_foreign_keys = ON`.** The deferred-check pragma set
  inside the transaction still failed at commit under modernc; disabling
  enforcement for a wholesale copy of an internally consistent
  `VACUUM INTO` snapshot is safe and unambiguous. It is toggled outside
  the transaction (a no-op inside one) and restored before the pooled
  connection is reused.

- **Excluded `schema_migrations` from the copy; guard schema-version
  match instead.** Restore copies rows, not DDL, so restoring an older
  checkpoint into a newer schema would desync the two. `checkSchemaMatch`
  fails closed unless the checkpoint's max applied version equals the
  live one, before any mutation. `schema_migrations` therefore stays at
  the live version (matching the unchanged DDL).

- **Local-only checkpoint, per §5.10 ("a local-only development
  checkpoint may come first") and the Wave 1A exit ("successful
  checkpoint restore, local-only acceptable").** The encrypted,
  digest-bound `BackupCheckpoint` model (§5.10) is explicitly out of
  scope.

- **Scope crosses daemon + app as one work unit (owner-chosen).** The
  issue requires proving "an already-cached client evicts", and the real
  client is Swift; the daemon-only Go proof covers the server contract,
  and the real-daemon convergence test
  (`restoreEvictsTheCachedCardDespiteVersionRollback`) is the faithful
  #165 + #162 gate. `store` is spine-owned territory, but `Checkpoint`/
  `Restore` add no domain-type, migration, interface, or `api/` change,
  so this is non-contract lane work, consistent with #83's dependency
  graph giving #165 no contract prerequisite.

## Refute-first pass (returned-object-trust + destructive path)

An independent fresh-context lens was given only the diff and the intent
and told to disprove the safety claims.

**Confirmed and fixed (all in the first draft of `Restore`):**

- **Foreign-key enforcement left OFF process-wide (HIGH).** The
  `PRAGMA foreign_keys = ON` cleanup defer used the request-scoped
  context. modernc keeps one pooled connection and never resets its
  session pragmas, so a request cancelled after `foreign_keys = OFF`
  skipped the re-enable and left FK enforcement off for every later
  query. Fix: bind both cleanup defers to `context.WithoutCancel`.
- **`restore_src` left attached (HIGH).** Same cancellable-context defer
  on `DETACH`; a skipped detach bricked every later restore
  (`ATTACH ... already in use`). Same fix, plus cleanup errors now
  surface through a named return instead of being swallowed.
- **Committed restore reportable as failed (MEDIUM).** The post-commit
  `SELECT` used the cancellable context, so a cancellation after commit
  returned an error over an already-committed rollback+rotation. Fix:
  read the post-restore state inside the transaction, before commit;
  there is no post-commit read left. Regression test
  `TestRestoreLeavesTheConnectionClean` pins FK-on and a second restore
  after the first.

**Survived the attack:**

- **Atomicity.** `Restore` holds the only pooled connection for its
  entire duration; every other reader/writer blocks until it closes. The
  epoch `UPDATE` shares the transaction with the data copy, so a partial
  or old-epoch read is unreachable.
- **Rotation cannot be bypassed.** The `UPDATE` runs unconditionally
  after the copy loop and inside the same transaction; a failure rolls
  back the whole thing. There is no committed restore without a rotated
  epoch (the `server_state` `CHECK (id = 1)` singleton guarantees the
  `WHERE id = 1` matches).
- **Fail-closed (data).** Schema-version mismatch, a missing checkpoint,
  or an unexpected table name each return an error before any mutation;
  the rollback defer is registered only after `BeginTx`, so no nil-tx
  rollback on early returns.
- **Injection / schema.** Table names come from `sqlite_master` and must
  pass `^[A-Za-z_][A-Za-z0-9_]*$` before interpolation; `schema_migrations`
  is excluded and its version pinned equal by `checkSchemaMatch`, so a
  column or table mismatch fails the `INSERT ... SELECT` and rolls back.

**Trust boundary — enforced at the endpoint plus read-time defense in
depth.** `Restore` does raw `INSERT ... SELECT`, bypassing the write-time
`Put` policy gates, so its rationale rests on the input being a
daemon-issued snapshot. A first draft left that assumption unenforced:
the review lens (Codex round 3) showed `/control/restore` passed any
caller-supplied path straight to `Store.Restore`, so a loopback caller
could restore from an arbitrary database. Fixed: the endpoint now
resolves the reference to an issued checkpoint name (`^[0-9a-f]{32}\.db$`)
inside its own checkpoint directory, `Lstat`ed as a regular file, so a
foreign path, traversal, or symlink is rejected. As defense in depth, the
store's read-time reconstruction re-gate (#52: `GetArtifact`/
`GetAttentionItem` re-derive `publish_eligible` against the approved
recipe set) still runs on every restored row. This is acceptable for a
local-only restore; a digest-verified checkpoint is part of the deferred
§5.10 model.

**Checkpoint directory is a credential surface.** A checkpoint carries the
whole store (device credentials, pairing rows), so the checkpoint
directory joins the store's backup surface: it is created and validated
owner-only before `store.Open` (so a rejected directory cannot strand a
never-paired store behind the #133 topic-key gate), and `storeSurface`
now excludes it from the persisted ntfy topic-key path (round 3), so the
derive-all key cannot be written where a copied checkpoint would carry
it.

**Residual (accepted).** If a cleanup pragma/detach itself fails for a
non-cancellation reason (disk error), the single connection is returned
to the pool dirty; `Restore` now returns that error so the failure is
loud, but discarding the poisoned connection is beyond `database/sql`'s
reach from outside a driver call. The realistic trigger (cancellation)
is closed; a hard I/O failure there means the store is already unhealthy.

## Revisit when

- The encrypted/digest-bound `BackupCheckpoint` (§5.10) is implemented:
  the schema-version guard becomes a digest+manifest verification, and
  the trust-boundary acceptance above should be re-derived against a
  verified-checkpoint threat model.
- A real `freesided` binary (§10) exists: the restore should be reachable
  operationally, not only through the dev control listener.

Follow-up: the #165 + #162 restore/client convergence gate (#83) is
satisfied by this unit's convergence test.
