# Store-owned SQLite substrate for publish mint-audit records

Work unit: #107 (kind:contract, fiat-scheduled 2026-07-17; deferred
from #80). Moves `publish.MintRecord` audit rows from the interim JSONL
file onto the store-owned SQLite surface per plan §5.9 (SQLite owns
audit) and §8 (typed relational rows).

## Decisions

- **Dependency direction: store-owned row struct, adapter in publish.**
  `store.MintAudit` is the store's own flat vocabulary (scope strings,
  not `publish.Permissions`); `publish.StoreRecorder` maps
  `MintRecord → store.MintAudit`. Rejected: store importing publish
  (inverts layering; would be the repo's first such edge) and moving
  MintRecord into domain (a heavier contract change the issue scopes
  out; the audit row is daemon-internal bookkeeping, not a synced
  domain entity).
- **Fully typed columns, no JSON body.** All of MintRecord's fields are
  closed scalars, so the row is fully columnar per §8; with no body
  there is no body/column cross-check machinery. Precedent:
  `device_credentials`, not the aggregate body-plus-keys shape. Rowid
  primary key and plain insert-only writes: a mint has no natural or
  idempotency key, and two byte-identical mints are two real events, so
  no `putImmutable`. Permission-scope columns are NOT NULL without a
  non-empty CHECK: empty legitimately means "not requested", and the
  audit surface records what happened rather than re-policing it.
- **No revision/sync columns; InternalTx write, non-Put name.** Audit
  is not a synced entity (#107 acceptance 1); the inbox/outbox and
  pairing rationale applies unchanged, and
  `TestRecordMintAuditInvisibleToSync` makes it executable.
- **`context.Background()` inside `StoreRecorder.RecordMint`.** The
  Recorder interface stays ctx-less by design (#80 contract). Rejected:
  storing a caller ctx in the adapter (a request-scoped cancellation
  mid-commit would fail mints on a deadline unrelated to audit
  durability; the local SQLite commit either lands or the mint fails).
- **JSONL recorder removed, not demoted.** Zero production callers ever
  existed (no daemon assembly composes publish yet), so #107
  acceptance 3 is satisfied vacuously: there is no production
  `mints.jsonl` to migrate. Demoting the fsync/symlink/TOCTOU machinery
  into tests would preserve durability engineering that audits nothing;
  the transaction commit supersedes it as the durability barrier. The
  opt-in live test now runs through `StoreRecorder`, so the real GitHub
  path exercises the production recorder.

## Refute-first pass (credential-adjacent mint path)

Target claim: the #80 invariant (a mint whose audit write fails must
fail; an unauditable token never circulates) survives the recorder
swap.

- Confirmed: `MintInstallationToken` calls `RecordMint` before
  returning the token and discards the token on error; the swap is
  entirely behind the `Recorder` interface and that call site is
  untouched.
- Confirmed by execution: `TestStoreRecorderFailsClosed` drives a full
  mint through the httptest fixture against a closed store and the mint
  fails with no token returned; `TestMintFailsWhenRecorderFails` (fake
  recorder) still covers the interface-level path.
- Residual, accepted: a commit-phase failure (as opposed to a
  begin-phase failure or callback error) is not separately forced in
  tests; `WriteInternal` propagates `Commit()` errors on the same
  return path the closed-store test proves.
- Rejected-by-verification: no re-gate is needed at audit read time.
  The row carries no trust bit (no recipe approval, no
  publish-eligibility, no token material representable), so
  reconstruction validates shape only; the #52 re-gate convention
  applies to decoded trust bits, which this table cannot hold.

Revisit when: daemon assembly composes the publish package (Wave 2);
wire `NewStoreRecorder` there. If a lane other than publish needs an
audit ledger, it gets its own typed table, not columns on this one.
