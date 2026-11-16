# Squash Migrations 0001 to 0120 Into a Baseline That Keeps Version 120

Owner decision of 2026-11-16 (#3252): squash the old migrations and delete
the tests that start below them, with the baseline "a couple weeks back".
The owner confirmed `0120` the same day, and that `prod` starts on a fresh
database (#3265). No database needs to migrate forward from below 120 with
a current build. The mechanism lives in `daemon/README.md` (Squashing
Migrations) and `internal/store/migrate.go`.

## Why

146 migrations and no baseline meant every data-rewrite test ran for as long
as the project did. The daemon test audit of 2026-11-16 found 106 store tests
that start from an older schema, and `internal/store` was 131 s of wall time
in CI's `rest` job. The tests below the baseline guarded migrations no
database still needs.

## Decisions

- **The baseline keeps version 120, over renumbering from 1.** Renumbering
  would make an old database at 146 and a new one at 27 compare wrongly in
  everything that reads `MAX(version)`: backups, restore, `undo-update`,
  and the pre-update check. Keeping the number leaves all of them unchanged.
- **The store accepts a database's old history as it is, over rewriting the
  ledger on open.** A database created before the squash keeps its rows 1 to
  120, and the store checks them against `squashed_history.txt` by name and
  digest. Rewriting them into the baseline's row would be a one-way data
  rewrite on every open, which `OpenReadOnly` can't do and which makes a
  database unreadable by the build before.
- **The list pins every row by name, over one digest of the whole history.**
  A single digest would be shorter, but the CI migration-history check needs
  each removed file's name and digest to tell a squash from a deletion.
- **A history that stops below the baseline is refused with a typed error
  naming `b7bd02f71`, over keeping a migration path.** The owner ruled out a
  path for such a database. The error names a build that migrates it, so the
  refusal is a step to take, not a dead end, and it fires before the store
  writes anything.
- **The Go data hooks go with the files they served.** All ten served
  migrations at or below `0098`, and no migration above the baseline has one,
  so `applyDataMigration` and `MigrationError`'s data flag went too. A future
  data rewrite adds its own hook.
- **The baseline is generated from a database the merge base built**, with
  the hooks run, and checked against a database built from every old file:
  schema text and every row. The sql text, not just names, is compared,
  because `ALTER TABLE` rewrites stored text.
- **The pre-rename dump moves to schema 146, over deleting its tests.** The
  dump sat at schema 63, below the baseline, but its tests in store, engine,
  and freesided cover readers of pre-rename rows, which #3266 owns. The last
  unsquashed build migrated it forward; its rows keep the old vocabulary, so
  the readers stay covered until #3266 decides. The one test of the task
  hook's output, `TestTaskMigrationReusesLegacySource`, went with the hook.
- **Squash rule: due at 60 migrations above the baseline,** with the new
  baseline at least two weeks behind head, once each instance that keeps its
  database is confirmed at or above it.

## Coverage Given Up

`TestLocalBackupProducerReplacesOlderSchemaCheckpoint` needed a checkpoint
without a column `0035` added, which can't be built at or above the baseline.
The local backup producer's replacement of an older-schema checkpoint has no
test now; a test at head minus one would need a migration that changes what
the producer reads.

`TestProductionReviewUpgradePreservesRequestTime` in `internal/integration`
upgraded an in-flight review across `0067` from a store rolled back to schema
66. The issue's audit listed store tests only and missed it.

## Refute-First Pass

A fresh-context reviewer and the test suite tried to break the change.

- **Confirmed and fixed:** a rewind helper read the deleted `0090` file (it
  now reads the guard from the baseline, which holds it verbatim); the
  integration upgrade test and the schema-63 pre-rename dump (above);
  `headVersion` counted files; `snapshotImage` failed for exactly the
  baseline on an empty cache.
- **Disproved:** a squash breaking restore (it copies every table except
  `schema_migrations` and compares `MAX(version)`, so a checkpoint from
  before the squash restores into a database created after it); a version
  reader counting ledger rows (backup, local backup, encrypted checkpoint,
  restore, database copy, and undo-update read `MAX(version)`); a write
  before the refusal (every open path validates the history first); a
  schema or row difference (sqlite3 and Go-built comparisons at 120 and at
  head).
- **Allowed:** nothing pins the baseline's content for a database created
  before the squash, since it never recorded the baseline's row; editing a
  merged migration is already forbidden, and every database created after
  the squash records and checks the digest. The prelaunch hint for a
  below-baseline database is the generic one, though its detail names the
  right build, and `olderSchemaRecovery` would name `0120_baseline.sql` for
  a pre-squash database at exactly 120; no instance is in either state. A
  pre-squash build refuses a database created after the squash, as any
  older build refuses a newer database.

Revisit when the next squash is due, or when a database below the baseline
turns up that its owner wants to keep.
