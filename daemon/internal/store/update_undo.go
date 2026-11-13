package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The undo of a failed update (issue #3108) puts the database an installer
// checkpointed back in place of the live one, after an updated daemon failed
// to start. It works on database files, never on a *Store: the live file may
// be at a schema a newer build migrated to, which this binary refuses to
// open, and Store.Restore refuses a checkpoint whose schema differs for the
// same reason.
//
// The files beside the live database <db> name each step, so a run killed at
// any point resumes from what is on disk instead of repeating it:
//
//	<db>.update-restore   the staged copy of the checkpoint, fresh sync_epoch
//	<db>.update-failed    the live database, set aside by the swap
//	<db>.update-failed-*  its -wal, -shm, and -journal files, set aside too
//
// The checkpoint file is deleted last, so its absence means the undo
// finished.

// ErrUpdateDrainLifted refuses an undo because the live drain log shows the
// drain the checkpoint holds was lifted. A lifted drain means the new build
// may have launched work and published, so putting the checkpoint back would
// discard real state.
var ErrUpdateDrainLifted = errors.New("the update drain was lifted after the checkpoint")

// ErrNoPendingUpdateUndo reports that neither the checkpoint nor a set-aside
// database exists: there is no undo to start or finish.
var ErrNoPendingUpdateUndo = errors.New("no pending update undo")

// sideFileSuffixes are SQLite's files beside a database. A leftover -wal or
// -journal would be replayed into whatever file next sits at the database's
// path, so each moves aside with the database it belongs to.
var sideFileSuffixes = []string{"-wal", "-shm", "-journal"}

// UpdateDrainLogEntry is one row of the update drain log with its row id,
// which orders the log and lets two copies of it be compared row by row.
type UpdateDrainLogEntry struct {
	ID         int64
	Transition domain.UpdateDrainTransition
}

// ReadUpdateDrainLog returns every update_drain_transitions row of the
// database at path in id order, without opening it as a Store, so it reads a
// database at any schema that has the table. A missing table or a row that
// does not validate is an error: the caller cannot rule out a lift from a
// log it cannot read.
func ReadUpdateDrainLog(ctx context.Context, path string) ([]UpdateDrainLogEntry, error) {
	db, err := openSchemaAgnostic(path)
	if err != nil {
		return nil, fmt.Errorf("read the update drain log of %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx,
		`SELECT id, state, build, forced, occurred_at FROM update_drain_transitions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read the update drain log of %s: %w", path, err)
	}
	defer func() { _ = rows.Close() }()
	var log []UpdateDrainLogEntry
	for rows.Next() {
		var (
			entry              UpdateDrainLogEntry
			state, build, when string
		)
		if err := rows.Scan(&entry.ID, &state, &build, &entry.Transition.Forced, &when); err != nil {
			return nil, fmt.Errorf("read the update drain log of %s: %w", path, err)
		}
		at, err := parseTime(when)
		if err != nil {
			return nil, fmt.Errorf("update drain row %d of %s: occurred_at: %w", entry.ID, path, err)
		}
		entry.Transition.State = domain.UpdateDrainState(state)
		entry.Transition.Build = domain.Digest(build)
		entry.Transition.OccurredAt = at
		if err := entry.Transition.Validate(); err != nil {
			return nil, fmt.Errorf("update drain row %d of %s: %w", entry.ID, path, err)
		}
		log = append(log, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the update drain log of %s: %w", path, err)
	}
	return log, nil
}

// CheckUpdateUndo is the gate on putting a checkpoint back: it allows the
// undo only when the drain log proves the new build never lifted the drain
// the checkpoint was taken under. It re-checks, from both files' own logs:
//
//   - the checkpoint's latest drain row is a draining row, so the checkpoint
//     was taken while an update drain held every launch;
//   - the live log holds that same row at the same id, so the live database
//     descends from the checkpoint;
//   - the live log has no row after it. A lifted row there returns
//     ErrUpdateDrainLifted; any other later row refuses too, because the
//     log moved in a way an update that never reopened cannot explain.
//
// The rule relies on two facts outside this file: a new build appends its
// lifted row at start before anything can launch
// (engine.UpdateDrain.LiftForBuild), and only an operator's drain command
// appends any other row.
func CheckUpdateUndo(checkpoint, live []UpdateDrainLogEntry) error {
	if len(checkpoint) == 0 {
		return errors.New("the checkpoint has no update drain row, so it was not taken under an update drain")
	}
	held := checkpoint[len(checkpoint)-1]
	if held.Transition.State != domain.UpdateDrainDraining {
		return fmt.Errorf("the checkpoint's latest update drain row %d is %s, not draining", held.ID, held.Transition.State)
	}
	at := -1
	for i, entry := range live {
		if entry.ID == held.ID {
			at = i
			break
		}
	}
	if at < 0 || !sameDrainTransition(live[at].Transition, held.Transition) {
		return fmt.Errorf("the live database does not hold the checkpoint's update drain row %d, so it does not descend from the checkpoint", held.ID)
	}
	later := live[at+1:]
	for _, entry := range later {
		if entry.Transition.State == domain.UpdateDrainLifted {
			return fmt.Errorf("%w: row %d lifted it at %s", ErrUpdateDrainLifted,
				entry.ID, entry.Transition.OccurredAt.Format(time.RFC3339))
		}
	}
	if len(later) > 0 {
		return fmt.Errorf("the live update drain log has %d rows after the checkpoint's, so a lift cannot be ruled out", len(later))
	}
	return nil
}

func sameDrainTransition(a, b domain.UpdateDrainTransition) bool {
	return a.State == b.State && a.Build == b.Build && a.Forced == b.Forced && a.OccurredAt.Equal(b.OccurredAt)
}

// UpdateUndoResult describes a finished undo.
type UpdateUndoResult struct {
	// CheckpointTime is the checkpoint file's modification time: changes
	// made after it were discarded.
	CheckpointTime time.Time
	// SchemaVersion is the restored database's migration version.
	SchemaVersion int
	// Resumed reports that an earlier, interrupted run had already set the
	// live database aside.
	Resumed bool
}

// UndoUpdate puts the checkpoint at checkpointPath back as the database at
// dbPath, after CheckUpdateUndo allows it, and deletes the checkpoint, the
// set-aside database, and the local backup checkpoint at
// <dbPath>.checkpoints once the restored file is in place. checkpointSHA256
// is the hex sha256 the installer recorded when it took the checkpoint; any
// other file is refused.
//
// The caller must hold the daemon lock on dbPath, so no daemon has the live
// file open and none starts during the swap.
//
// It resumes from what is on disk:
//
//   - nothing set aside: check the drain logs, stage, swap, and finalize;
//   - the live database set aside, nothing at dbPath: the check passed before
//     the first rename, so finish the swap and finalize;
//   - both present: the swap finished only if the live database is the
//     staged copy (restoredFromCheckpoint). Finalize then; otherwise refuse
//     and keep both, because the live file may be one a daemon created at
//     the empty path while the undo was dead, and the set-aside file may be
//     the only copy of the real data.
func UndoUpdate(ctx context.Context, dbPath, checkpointPath, checkpointSHA256 string) (UpdateUndoResult, error) {
	failedPath := dbPath + ".update-failed"
	stagedPath := dbPath + ".update-restore"
	checkpointInfo, checkpointErr := os.Lstat(checkpointPath)
	failedExists, err := pathExists(failedPath)
	if err != nil {
		return UpdateUndoResult{}, err
	}
	switch {
	case errors.Is(checkpointErr, os.ErrNotExist) && !failedExists:
		return UpdateUndoResult{}, fmt.Errorf("%w: neither %s nor %s exists", ErrNoPendingUpdateUndo, checkpointPath, failedPath)
	case errors.Is(checkpointErr, os.ErrNotExist):
		return UpdateUndoResult{}, fmt.Errorf("%s exists but the checkpoint %s does not, so it is not this undo's; inspect it",
			failedPath, checkpointPath)
	case checkpointErr != nil:
		return UpdateUndoResult{}, fmt.Errorf("checkpoint: %w", checkpointErr)
	case !checkpointInfo.Mode().IsRegular():
		return UpdateUndoResult{}, fmt.Errorf("checkpoint %s is not a regular file", checkpointPath)
	}
	if err := checkFileSHA256(checkpointPath, checkpointSHA256); err != nil {
		return UpdateUndoResult{}, err
	}
	result := UpdateUndoResult{CheckpointTime: checkpointInfo.ModTime().UTC(), Resumed: failedExists}

	liveExists, err := pathExists(dbPath)
	if err != nil {
		return UpdateUndoResult{}, err
	}
	switch {
	case !failedExists:
		if !liveExists {
			return UpdateUndoResult{}, fmt.Errorf("live database %s does not exist", dbPath)
		}
		if err := checkUndoLogs(ctx, checkpointPath, dbPath); err != nil {
			return UpdateUndoResult{}, err
		}
		// A run killed before its first rename can leave a staged copy or,
		// from an earlier finalize, set-aside side files; neither is in use.
		if err := removeFiles(append(sideFiles(failedPath), stagedPath)...); err != nil {
			return UpdateUndoResult{}, err
		}
		if err := stageUpdateRestore(ctx, checkpointPath, stagedPath); err != nil {
			return UpdateUndoResult{}, err
		}
		if err := swapInUpdateRestore(dbPath, failedPath, stagedPath); err != nil {
			return UpdateUndoResult{}, err
		}
	case !liveExists:
		// Always re-stage: a staged file found here may be a copy a killed
		// re-stage left torn or without its new sync_epoch.
		if err := removeFiles(stagedPath); err != nil {
			return UpdateUndoResult{}, err
		}
		if err := stageUpdateRestore(ctx, checkpointPath, stagedPath); err != nil {
			return UpdateUndoResult{}, err
		}
		if err := swapInUpdateRestore(dbPath, failedPath, stagedPath); err != nil {
			return UpdateUndoResult{}, err
		}
	default:
		swapped, err := restoredFromCheckpoint(ctx, checkpointPath, dbPath)
		if err != nil {
			return UpdateUndoResult{}, err
		}
		if !swapped {
			return UpdateUndoResult{}, fmt.Errorf("%s exists but %s was not restored from the checkpoint, so it is not this undo's; inspect it",
				failedPath, dbPath)
		}
	}

	result.SchemaVersion, err = ReadSchemaVersion(ctx, dbPath)
	if err != nil {
		return UpdateUndoResult{}, fmt.Errorf("read the restored schema version: %w", err)
	}
	// The local backup checkpoint goes too: the failed build's first
	// maintenance pass may have replaced it with one at its newer schema,
	// which the restored build refuses to start over. Any older one is
	// superseded by the restored database, and the restored build writes a
	// fresh one when it starts.
	_, localBackupPath, _, err := defaultLocalBackupPaths(dbPath)
	if err != nil {
		return UpdateUndoResult{}, err
	}
	// The local backup and the set-aside database first and the checkpoint
	// last, so a run killed in between still finds the checkpoint, sees both
	// databases, and finishes here.
	if err := removeFiles(append(sideFiles(failedPath), localBackupPath, failedPath, checkpointPath)...); err != nil {
		return UpdateUndoResult{}, err
	}
	if err := syncDir(filepath.Dir(dbPath)); err != nil {
		return UpdateUndoResult{}, err
	}
	return result, nil
}

func checkUndoLogs(ctx context.Context, checkpointPath, dbPath string) error {
	checkpointLog, err := ReadUpdateDrainLog(ctx, checkpointPath)
	if err != nil {
		return err
	}
	liveLog, err := ReadUpdateDrainLog(ctx, dbPath)
	if err != nil {
		return err
	}
	return CheckUpdateUndo(checkpointLog, liveLog)
}

// stageUpdateRestore copies the checkpoint to a new owner-only file at
// stagedPath and gives the copy a fresh sync_epoch, as Store.Restore does, so
// clients drop caches built on the discarded history. VACUUM INTO writes a
// rollback-journal database, so the committed copy has no side files.
func stageUpdateRestore(ctx context.Context, checkpointPath, stagedPath string) error {
	if err := CopyDatabaseFile(ctx, checkpointPath, stagedPath); err != nil {
		return fmt.Errorf("stage the checkpoint: %w", err)
	}
	epoch, err := randomEpoch()
	if err != nil {
		return errors.Join(err, os.Remove(stagedPath))
	}
	if err := setSyncEpoch(ctx, stagedPath, epoch); err != nil {
		return errors.Join(fmt.Errorf("stage the checkpoint: %w", err), os.Remove(stagedPath))
	}
	if err := syncFile(stagedPath); err != nil {
		return errors.Join(fmt.Errorf("stage the checkpoint: %w", err), os.Remove(stagedPath))
	}
	return nil
}

func setSyncEpoch(ctx context.Context, path, epoch string) error {
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+
		"?mode=rw&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	result, err := db.ExecContext(ctx, `UPDATE server_state SET sync_epoch = ? WHERE id = 1`, epoch)
	if err == nil {
		var n int64
		if n, err = result.RowsAffected(); err == nil && n != 1 {
			err = fmt.Errorf("server_state has %d rows with id 1, want 1", n)
		}
	}
	return errors.Join(err, db.Close())
}

// swapInUpdateRestore sets the live database and its side files aside, then
// renames the staged copy into place. The live file goes first: from then
// on the failed file's presence tells a resumed run the check already
// passed. Its side files follow before the rename, because SQLite would
// replay a leftover -wal or -journal into the restored file.
func swapInUpdateRestore(dbPath, failedPath, stagedPath string) error {
	if exists, err := pathExists(dbPath); err != nil {
		return err
	} else if exists {
		if err := os.Rename(dbPath, failedPath); err != nil {
			return fmt.Errorf("set the live database aside: %w", err)
		}
	}
	live, failed := sideFiles(dbPath), sideFiles(failedPath)
	for i := range live {
		if err := os.Rename(live[i], failed[i]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("set the live database's %s aside: %w", sideFileSuffixes[i], err)
		}
	}
	dir := filepath.Dir(dbPath)
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.Rename(stagedPath, dbPath); err != nil {
		return fmt.Errorf("put the checkpoint in place: %w", err)
	}
	return syncDir(dir)
}

// restoredFromCheckpoint reports whether the live database is the copy this
// undo staged from the checkpoint: the same update drain log and schema
// version, under a different sync_epoch, which only staging gives it. A
// database a daemon created at the live path while the swap was half done
// has an empty drain log, and one any daemon reopened after the swap has a
// later drain row or a newer schema, so neither passes.
func restoredFromCheckpoint(ctx context.Context, checkpointPath, dbPath string) (bool, error) {
	checkpointEpoch, err := readSyncEpoch(ctx, checkpointPath)
	if err != nil {
		return false, err
	}
	liveEpoch, err := readSyncEpoch(ctx, dbPath)
	if err != nil {
		return false, err
	}
	if checkpointEpoch == liveEpoch {
		return false, nil
	}
	checkpointLog, err := ReadUpdateDrainLog(ctx, checkpointPath)
	if err != nil {
		return false, err
	}
	liveLog, err := ReadUpdateDrainLog(ctx, dbPath)
	if err != nil {
		return false, err
	}
	if len(checkpointLog) != len(liveLog) {
		return false, nil
	}
	for i := range checkpointLog {
		if checkpointLog[i].ID != liveLog[i].ID ||
			!sameDrainTransition(checkpointLog[i].Transition, liveLog[i].Transition) {
			return false, nil
		}
	}
	checkpointVersion, err := ReadSchemaVersion(ctx, checkpointPath)
	if err != nil {
		return false, err
	}
	liveVersion, err := ReadSchemaVersion(ctx, dbPath)
	if err != nil {
		return false, err
	}
	return checkpointVersion == liveVersion, nil
}

func readSyncEpoch(ctx context.Context, path string) (string, error) {
	db, err := openSchemaAgnostic(path)
	if err != nil {
		return "", fmt.Errorf("read the sync epoch of %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	var epoch string
	if err := db.QueryRowContext(ctx, `SELECT sync_epoch FROM server_state WHERE id = 1`).Scan(&epoch); err != nil {
		return "", fmt.Errorf("read the sync epoch of %s: %w", path, err)
	}
	return epoch, nil
}

func checkFileSHA256(path, want string) error {
	decoded, err := hex.DecodeString(want)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("checkpoint sha256 %q is not 64 hex digits", want)
	}
	file, err := os.Open(path) //nolint:gosec // G304: the operator-named checkpoint
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	defer func() { _ = file.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if got := sum.Sum(nil); !bytes.Equal(got, decoded) {
		return fmt.Errorf("checkpoint %s has sha256 %s, not the recorded %s", path, hex.EncodeToString(got), want)
	}
	return nil
}

func sideFiles(path string) []string {
	files := make([]string, len(sideFileSuffixes))
	for i, suffix := range sideFileSuffixes {
		files[i] = path + suffix
	}
	return files
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func removeFiles(paths ...string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	return nil
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: a file this undo created
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// syncDir makes the renames and removals in dir durable before the next
// step relies on them.
func syncDir(dir string) error {
	file, err := os.Open(dir) //nolint:gosec // G304: the database's own directory
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return errors.Join(file.Sync(), file.Close())
}
