# Store Migration Tests Start From Cached Images

Work unit: #2746 (tracker #2754, Test speed). Mandatory note: it is Tier A
work, because it changes where migration and restore gate tests start,
which is what they feed the gate. It extends the snapshot cache that
PR #2675 (#2665) added in
`daemon/internal/store/migration_snapshot_internal_test.go`.

## Decision: Start From the Cache, and Refuse a Prefix Built From Empty

Chose to start every `internal/store` migration test that rebuilt a
schema prefix from empty from the snapshot cache (`openRawThrough`,
`copySnapshotThrough`, or the new `copySnapshotAt`), and five external
restore and revision tests from the head template (`storetest.Open`),
over leaving each test to build its own start:

- **A cached image proves the same thing.** It is the same embedded
  migration prefix applied by the same `migrate`, so the migration under
  test still runs for real against a database at the real prior head.
  `TestMigrationSnapshotsMatchInProcessMigration` pins every cached
  version equal, in schema and applied-migration records, to the chain
  stepped in process from empty, and `openRawThrough` checks the version
  it hands out. Before converting, a scratch check compared the old
  hand-built start with the cached image at every version this unit uses
  (7 to 129) on schema, every row of every table, and three pragmas; all
  matched.
- **The template is what `Open` would build.** For the five external
  tests (`TestCheckpointRestoreRotatesEpochAndRollsBackData`,
  `TestRestoreLeavesTheConnectionClean`,
  `TestRestoreRoundTripsComprehensionRows`, `TestCheckpointFileIsOwnerOnly`,
  `TestRevisionChangedClosesOnEveryCursorMove`), the head template is the
  schema `Open` builds from empty, unseeded, and its first open seeds the
  epoch as a fresh file would. None of the five asserts anything about a
  first open: their epoch and revision checks are relative to the state
  they read after opening, and the file-mode check is on the checkpoint,
  not the database. `TestTemplateDatabaseIsCompleteAndSeedsIndependentEpochs`
  covers the template itself.
- **One older-schema image per process.** `writeOlderSchemaDatabase`
  builds its image once with `sync.OnceValues` under
  `context.Background`, because the first caller's `t.Context` ends with
  that test; each caller still writes its own file.
- **No more migration lists by name.** Four files listed every later
  migration by name to exclude it and had to be edited whenever a
  migration landed; the `before` name replaces each list.

Measured with a temporary trace in `migrate` at `-parallel 4`, the
package went from 2,136 `migrate` calls, 15,296 applications, and 79
from-empty calls to 2,129, 13,644, and 53: 1,652 fewer applications,
about 11% of the package's migration work.

## Decision: The Ratchet Lives in `migrateThrough`

`migrateThrough` now fails the test, naming `openRawThrough` and
`copySnapshotThrough`, when the database has no applied migrations. Chose
this over a lint rule or a review note because every from-empty prefix in
the package went through `migrateThrough` or a hand-built copy of it, and
the failure message tells a new test's author what to use. A test that
hand-builds a prefix without `migrateThrough` is not caught; the deleted
lists were the only such cases.

## What Stays From Empty, and Why

- **The chain proofs.** `TestMigrateFreshAndIdempotent` and
  `TestMigrationSnapshotsMatchInProcessMigration` prove the chain from
  empty; starting them from a cache would assume what they prove.
- **`Open`'s own tests.** `TestOpenDefaultBusyTimeout`, `TestOpenPragmas`,
  and the other `TestOpen*` tests that create a fresh file have `Open` on
  an empty file as their subject, as does the template test.
- **The synthetic `mapFS` tests.** `TestFailingMigrationRollsBack` and
  `TestMigrateRejectsDivergedHistory` migrate substitute files, not the
  embedded chain.

## Left for the Owner

`LocalBackupProducer.writeRestoreTest` migrates an in-memory database from
empty on every `Maintain`: 31 calls and about 4,000 applications, roughly
a quarter of the package's migration work. It is production code on a
Tier A path (`internal/store/local_backup*.go`), so changing it is the
owner's call and a separate unit.

Revisit when the package's migration work grows back past its level
before this unit, or when `writeRestoreTest` changes how it builds its
restore target.
