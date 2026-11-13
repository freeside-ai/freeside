# Undo a Failed Update From a Checkpoint

Issue #3108. The Mac installer now checkpoints the database before it
replaces a running daemon with a changed build, waits a bound for the new
build to lift the update drain, and otherwise puts the checkpoint and the
previous app back. Tier A: `freesided undo-update` replaces the live
database, which holds credentials.

## Decisions

- **Chose a file swap over `Store.Restore`.** `Store.Restore` opens the live
  store and refuses a backup whose schema differs, and the old build can't
  open a database the failed build migrated forward. The undo works on files
  with the new build's binary: it reads both drain logs schema-agnostically,
  stages a copy of the checkpoint, moves the live file and its side files
  aside, and renames the copy into place. It still gives the copy a fresh
  `sync_epoch`, as `Store.Restore` does, so clients drop caches built on the
  discarded history.
- **Chose the drain log as the gate, over the checkpoint's age or the
  daemon's exit status.** A new build appends its lifted row at start
  (`engine.UpdateDrain.LiftForBuild`) before any loop can launch, so a live
  log with nothing after the checkpoint's draining row proves the new build
  never launched or published anything. A later lifted row means exit 3 and
  nothing restored; any other later row also refuses, because an update that
  never reopened cannot explain it. Revisit when anything other than a new
  build's start and `drain -cancel` appends lifted rows, or anything other
  than an installer's drain appends draining rows during an update.
- **Chose a plaintext `freesided snapshot` in an owner-only directory over an
  encrypted checkpoint.** The checkpoint lives in the tier's `0700` state
  directory beside the live database, which holds the same credentials in
  the same form, and exists only while an update is unresolved. Encryption
  would add a key to manage for no reduction in exposure.
- **Chose the app's `FreesideMac daemon stop|start` over `launchctl`.** The
  app owns the SMAppService registration, its marker, and the
  operator-disabled flag, so stopping and starting through it keeps them
  consistent. `undo-update` takes the daemon lock, so if launchd keeps
  restarting the daemon anyway, the undo refuses and the installer keeps the
  checkpoint instead of swapping under a live daemon.
- **Chose a 300-second bound, configurable with
  `FREESIDE_MAC_REOPEN_TIMEOUT`.** A daemon still migrating at the bound is
  undone too; SQLite rolls back the interrupted migration, and the operator
  re-runs with a larger bound. An unbounded wait would leave a failed build
  in place with no undo.
- **Chose to delete the failed database once the checkpoint is back, over
  keeping it.** It holds credentials and the failed build's writes, which
  the previous build can't open. Keeping it would leave a second credential
  copy nobody owns. An undo stopped between the two leaves it, and the
  installer refuses a new checkpoint while one is there.
- **Chose to delete the local backup checkpoint on undo, over keeping or
  saving and restoring it.** The dev-tier exercise found that the failed
  build's first maintenance pass rewrote `<db>.checkpoints/latest.backup`
  at its newer schema, and the restored build refuses to start over a
  newer-schema checkpoint. Anything in that file newer than the update
  checkpoint came from the failed build, and anything older is superseded
  by the restored database, so `undo-update` deletes it before the update
  checkpoint and the restored build writes a fresh one when it starts.
  Saving a copy at checkpoint time would add a second artifact to verify
  and resume for no data the restored database lacks. A second
  fresh-context refute pass found no defect. Revisit if anything else
  beside the database starts carrying a version the previous build checks
  at start: the publish janitor snapshot and journal are version-gated
  today, both at version 1.
- **Chose the checkpoint's absence as the "restored" marker, over the plan's
  `db-restored` file.** `undo-update` deletes the checkpoint last, so there
  is no window between a finished restore and the marker being written. A
  re-run with no checkpoint skips straight to putting the app back.
- **Chose a positive lineage check to tell a finished swap from a foreign
  live file, over the plan's "both present means finalize" and over a
  `sync_epoch` comparison alone.** With the live database and
  `<db>.update-failed` both present, the swap finished only if the live file
  has the checkpoint's drain log and schema version under a different
  `sync_epoch`, which only staging gives it. The epoch alone was the first
  version and the refute-first review broke it (finding 1 below).
- **Kept one PR over the plan's daemon and installer split.** The owner's
  `Handle #3108` covered the whole issue, and the split was a proposal that
  was not applied.

## Refute-First Findings

One fresh-context reviewer tried to break the diff before the first commit.

- **Confirmed and fixed (high): a resumed undo could delete the real
  database.** An undo killed between its two renames releases the lock with
  nothing at the live path. A daemon starting then (`store.Open` creates a
  missing file) leaves an empty database whose random epoch differs from
  the checkpoint's, so the next run finalized and deleted the set-aside
  data, and the installer's resume read that daemon as a reopened new build.
  The reviewer reproduced it. Fixed by the lineage check above, a
  regression test, and an installer rule that a set-aside database always
  routes a re-run to the undo.
- **Confirmed and fixed (medium): a cut-short checkpoint delete stranded
  later runs.** A partial `rm -rf` could keep `swapped` without the
  checkpoint, so every later run died before building. The `swapped` marker
  is now deleted first, and a missing `update.env` value no longer trips
  errexit before the "incomplete" message.
- **Confirmed and fixed (low): a resume re-stage was not atomic, and an
  interrupt between the checkpoint and the swap kept the checkpoint.** The
  resume path now always re-stages from the verified checkpoint, and the
  pre-swap rollback deletes the checkpoint with the drain cancel.
- **Allowed by decision (medium): an old daemon that cannot start after the
  restore blocks re-runs.** It needs a kill in a seconds-wide window and the
  operator opening the failed build, which then migrates the restored
  database. No data is lost; the message now says to remove the checkpoint
  directory and install a build that can open the database.
- **Allowed by decision (medium): a run killed between the swap and opening
  the new app undoes on the re-run.** The new build never ran, so the
  restore discards the drained old daemon's writes since the checkpoint.
  The window is seconds, the drained daemon launches nothing, and the
  contract accepts losing post-checkpoint writes on an undo; the installer
  prints the checkpoint time.
- **Disproved or declined (low): a `drain -status` poll can collide with a
  starting daemon's lock.** That poll is the existing #3104 readiness read,
  and launchd restarts a daemon that exits on a held lock.
- **Checked and held:** read-only reads see a crashed daemon's `-wal`;
  `VACUUM INTO` writes a rollback-journal copy; the lock spans the whole
  undo; the drain-log gate matches `LiftForBuild`; the sha256 is checked
  before any read or write; permissions are `0700` and `0600`.
