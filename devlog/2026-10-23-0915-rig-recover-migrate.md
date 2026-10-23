# Rig Recover Migrates An Older Build's Journal Only On Opt-In

Issue #2201, found in the Wave 8 exit run (#1613). Decided by the agent
from the issue contract, under the mandatory-note rule for destructive-path
work.

## Context

- **Recovery opens the database only for review resources.** A stale rig
  lease's manifest is read without the database. Recovery opens the
  recorded database only when the manifest records review volumes or
  networks, to tear them down through the review journal
  (`productionRigHost.RecoverReviewResources`,
  `daemon/cmd/freesided/rig.go`).
- **That open required this build's exact schema.** It used
  `store.OpenExisting`, so a lease an older build left behind blocked
  every deputy start machine-wide, and the refusal named `rig recover`,
  which failed the same way. The only way out was to build the older
  commit.

## Decisions

1. **Keep "recovery never migrates" as the default; add `-migrate` to
   `freesided rig recover`.** With it, recovery opens the database through
   the new `store.MigrateExisting`, which never creates a file and migrates
   it to head as the daemon's next start would. Without it, the refusal
   names the database's schema, this build's schema, the newest migration
   file at the database's schema, and the two ways out. Chose an explicit
   flag because migrating is one-way: after it, a build at the older schema
   can no longer open that database, so an operator who meant to finish
   with the older build must not lose that option to a recovery command.
2. **The deputy passes migrate for its own root's stale lease.** The
   operator is running this build on that root, and the start opens the
   same database through `store.Open`, which migrates it anyway. A start
   that fails between recovery and that open (the acquisition write, the
   App authority check, the idle proof) still leaves the database
   migrated; the next start of this build would migrate it too, so the
   operator loses nothing they had not chosen by running it there. A stale
   lease for another root still refuses the start and names `rig recover`,
   because nothing says the operator meant to move that root to this
   build.
3. **The store returns a typed `*store.SchemaMismatchError`** from
   `OpenExisting` and `OpenReadOnly`, with the existing message text, so
   the command composes its refusal without parsing strings.

Rejected:

- **Migrate whenever `-confirm` is given.** `-confirm` authorizes removing
  the recorded resources, not a one-way schema change to the database.
- **Read the journal through the older schema.** It would need the store
  to interpret rows through a schema other than the binary's, which
  `OpenExisting` exists to forbid.
- **Record the build or schema in the rig manifest.** It bumps the
  manifest version, and the schema is already readable from the database.

## Refute-First Pass

One fresh-context reviewer tried to prove the change wrong.

- **Migration without opt-in: disproved.** `-migrate` defaults to false;
  the deputy passes true only after refusing any lease whose state root is
  not its own, and `RigManifest.validate` pins the database path to
  `<state-root>/freeside.db`, the deputy's own database.
- **A missing database is created: disproved.** Both modes open with
  `mode=rw`; the test runs both.
- **Concurrent access during migration: disproved.** `recoverStaleRig`
  holds the stale lease and the database lock for the whole recovery.
- **A newer database is migrated or misreported: disproved.**
  `validateAppliedMigrations` refuses "binary older than database" before
  any schema mismatch, in both the open and the migrate path.
- **`MigrateExisting` diverges from `Open`: disproved.** It differs only in
  `mode=rw` and the WAL pragma, and WAL mode persists in any database
  `Open` created.
- **The deputy comment overstated the start's guarantee: confirmed, fixed**
  in the comment and decision 2 above.
- **An empty file at the database path is initialized: allowed.** It is
  not a missing database, `store.Open` would initialize it the same way,
  and recovery still fails closed on the unrecovered resources.
- **The refusal rendered "migration migration 0000" for an empty history:
  confirmed, fixed** to name no migration.

Revisit when the store gains cross-version read compatibility (#1266
covers the API side), which would let recovery read an older journal
without migrating it.
