---
run: manual
stage: encrypted-backup-checkpoint
date: 2026-07-30
branch: feat/encrypted-backup-checkpoint
---

# Deliver the Encrypted, Digest-Bound Backup Checkpoint

Work unit: #305. Scope: `daemon/`, `scripts/run-real-work.sh`, and `devlog/`.

## Decisions

**Use one authenticated encrypted envelope for the complete local
checkpoint.** The on-disk `latest.backup` contains only a format version, key
identifier, nonce, and AES-256-GCM ciphertext. The encrypted payload contains
the `BackupCheckpoint` metadata followed by the standalone SQLite snapshot.
Authenticated metadata binds the checkpoint ID, captured sync epoch and server
revision, SQLite snapshot digest, artifact-manifest digest, creation and
completion times, and restore-test time. Restore authenticates the envelope,
checks the plaintext snapshot digest, reconstructs the database under current
trust policy, recomputes the artifact-manifest digest, and verifies every
referenced artifact before copying rows.

Rejected:

- Leaving checkpoint metadata in the clear. It held no credential values, but
  "encrypted checkpoint" is simpler and stronger when the complete checkpoint
  contract is ciphertext.
- Encrypting the SQLite file without authenticating metadata. That permits a
  digest, revision, or lifecycle timestamp to be substituted independently of
  the snapshot.
- Reusing the ntfy topic key. Backup confidentiality and notification
  capability routing are separate concerns with different recovery and
  rotation lifecycles.

**Keep the Phase 1A key beside, not inside, the checkpoint surface.**
`<db>.backup-encryption.key` is a generated 32-byte owner-only regular file.
Loading rejects symlinks, hard links, malformed length, and widened
permissions. A missing key beside an existing encrypted checkpoint fails
closed rather than silently making that checkpoint irrecoverable. An upgrade
with only the legacy plaintext checkpoint may create its first key, produce
and restore-test the encrypted replacement, then delete `latest.db` and
`restore-test.db`. Portable control-plane key wraps and operator recovery
remain #266's scope.

**Keep plaintext snapshots transient and use memory for reads and restore.**
Health and restore deserialize authenticated SQLite bytes into an in-memory
database. Restore copies its rows into one target transaction and rotates the
live sync epoch in that same transaction, preserving the checkpoint's captured
epoch as evidence while retaining §5.10's rollback rule. Production snapshot
creation uses SQLite's online-backup API to copy the WAL-coherent live state
into an in-memory database, then serializes that database directly into the
encrypted envelope. It never writes the production checkpoint plaintext to
disk. Maintenance still removes a crash-left `.latest-*.db` from an older or
interrupted producer before doing other work.

Rejected:

- Treating the restored live epoch as identical to the checkpoint epoch. That
  would satisfy a literal reading of "preserves sync epoch" but violate the
  binding §5.10 rule that rollback issues a fresh epoch. The round-trip proof
  instead checks that authenticated checkpoint metadata preserves the captured
  epoch and revision, while live restore preserves revision and rotates epoch.
- Serializing the live WAL connection directly. SQLite documents that as the
  main database file's bytes, which excludes committed WAL pages and did not
  produce a usable standalone snapshot. Online backup materializes those pages
  into the in-memory destination first.
- A durable plaintext restore-test database. The authenticated
  `restore_tested_at` now records a real in-memory restore of the exact
  ciphertext generation without keeping a second credential-bearing SQLite
  file.

**Extend backup health without changing #317's other verdicts.** `BackupHealth`
now has an independent `encryption` status. The encrypted source reports it
healthy only after authentication and digest checks; the compatibility
plaintext evaluator always reports it unhealthy. Currency, artifact closure,
and restore-test age keep their existing inputs, windows, and admission
semantics.

**Retire the waiver at the production configuration boundary.** Supplying
`-backup-encryption-waiver-repository-id` now returns
`ErrBackupEncryptionWaiverUnsupported`; the real-run harness no longer
requires or passes it. The store also rejects every new waiver-bearing
admission. Historical waiver fields remain structurally validated but are
otherwise inert during reconstruction, so in-flight work from the prior build
can recover once all four backup-health dimensions pass. Healthy encrypted
backup evidence also supersedes legacy waived-posture notices.

No schema migration is required. Checkpoint metadata belongs to the encrypted
artifact rather than the live database, and the existing local backup marker
tables remain readable only for the compatibility evaluator.

## Refute-First Verification

- **Confirmed and fixed:** an early design decrypted to a named temporary
  SQLite file for every health read. A crash could leave recoverable plaintext.
  Health and restore now use SQLite's in-memory deserialize interface.
- **Confirmed and fixed:** the first snapshot producer still used a short-lived
  owner-only plaintext file. The final path uses SQLite online backup plus
  in-memory serialization, so the checkpoint has no plaintext disk generation.
- **Confirmed and fixed:** direct `O_CREATE` key publication could strand a
  zero-length or partial final key after a crash or write failure. Key creation
  now writes, restricts, and syncs a same-directory temporary file before an
  atomic rename publishes the complete 32-byte key, followed by directory sync.
- **Confirmed and fixed:** rejecting waiver configuration while still requiring
  it to reconstruct an old waiver-bearing admission made upgrade recovery
  impossible. Reconstruction now treats the legacy field as inert under the
  four-part encrypted health gate, while the write boundary rejects it for new
  admissions; encrypted health likewise supersedes old waiver-posture notices.
- **Confirmed and fixed:** the new encryption-health refusal was absent from
  the engine's mutable-policy classification, so startup recovery could exit
  before maintenance replaced a legacy plaintext checkpoint. A not-yet-
  encrypted checkpoint now holds recovery for a later reconcile pass, while
  malformed records and immutable binding failures remain fatal.
- **Confirmed and fixed:** a crash after publishing or restore-testing the
  encrypted checkpoint could strand legacy plaintext database files and WAL
  sidecars because cleanup ran only on the checkpoint-creation path. Every
  successful maintenance pass now retries the idempotent cleanup, including
  stale temporary snapshot sidecars.
- **Confirmed and fixed:** concurrent first-start daemons could both observe
  an absent key and let `rename` replace the first published key with the
  second. Key publication now uses the platform's atomic no-replace rename;
  every loser discards its candidate and loads the winning durable key.
- **Confirmed and fixed:** crash-left `.latest-*.backup` files and the prior
  producer's `.restore-test-*.db` files were outside the stale temporary
  sweep. The validated cleanup now removes encrypted temporaries plus both
  legacy plaintext temporary families and their SQLite sidecars.
- **Confirmed and fixed:** cleanup validated only the authenticated snapshot
  identity before deleting the legacy fallback, not its artifact manifest
  under current reconstruction rules. Maintenance now runs the full manifest
  gate first; a policy mismatch refreshes the checkpoint, and a failed refresh
  leaves both the encrypted checkpoint and plaintext fallback untouched.
- **Confirmed and fixed:** a cleartext envelope header would have left
  checkpoint metadata outside encryption. The complete `BackupCheckpoint`
  metadata moved inside ciphertext.
- **Rejected by verification:** wrong keys, ciphertext changes, and an
  authenticated false snapshot digest do not reach restore. Tests require the
  authentication or digest sentinel and query encryption as unhealthy.
- **Rejected by verification:** the encrypted artifact does not contain the
  SQLite header or a seeded credential-bearing verifier value.
- **Rejected by verification:** symlinked, hard-linked, missing, malformed, or
  non-private key files cannot become the data key.
- **Rejected by verification:** restore does not retain post-checkpoint rows,
  regress the captured revision incorrectly, or reuse the pre-restore live
  epoch.
- **Rejected by verification:** cleanup cannot remove the legacy fallback
  before proving the encrypted checkpoint. Tampering aborts maintenance, and
  a widened manifest policy must install and restore-test a replacement before
  cleanup; either failure leaves the fallback in place.

Revisit when: #266 introduces portable replication and replaces the local raw
data key with host-specific and operator recovery wraps.
