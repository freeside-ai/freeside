package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultLocalCheckpointRefreshInterval renews the checkpoint halfway
	// through its health window, leaving time for a transient production
	// failure to recover before unattended admission closes.
	DefaultLocalCheckpointRefreshInterval = 12 * time.Hour
	// DefaultLocalRestoreTestRefreshInterval applies the same safety margin to
	// the monthly restore-test window.
	DefaultLocalRestoreTestRefreshInterval = 15 * 24 * time.Hour
	// DefaultLocalBackupPollInterval bounds how long a recovered producer
	// failure can delay the next maintenance attempt.
	DefaultLocalBackupPollInterval = time.Hour
)

// LocalBackupProducer maintains the provisional owner-only checkpoint and its
// restored test copy. The encrypted checkpoint can replace this producer
// without changing BackupHealthSource or admission policy.
type LocalBackupProducer struct {
	store *Store
	files *LocalBackupFiles
	now   func() time.Time
}

// NewProducer builds the producer paired with this file set's health source.
func (f *LocalBackupFiles) NewProducer(store *Store) (*LocalBackupProducer, error) {
	if store == nil {
		return nil, errors.New("local backup producer: nil store")
	}
	if f == nil {
		return nil, errors.New("local backup producer: nil backup files")
	}
	return &LocalBackupProducer{
		store: store, files: f, now: time.Now,
	}, nil
}

// Maintain refreshes evidence that is missing, incompatible with the live
// store, or beyond its safety-margin interval.
func (p *LocalBackupProducer) Maintain(ctx context.Context) error {
	if err := ensurePrivateBackupDirectory(p.files.dir); err != nil {
		return fmt.Errorf("local backup producer: %w", err)
	}
	var current BackupHealthContext
	if err := p.store.Read(ctx, func(tx *ReadTx) error {
		var err error
		current, err = tx.backupHealthContext(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("local backup producer: read live state: %w", err)
	}

	checkpoint, found, err := inspectBackupDatabase(
		ctx, p.files.checkpointPath, false, nil, nil)
	if err != nil {
		return fmt.Errorf("local backup producer: inspect checkpoint: %w", err)
	}
	if !found || backupSnapshotDue(
		checkpoint, current, p.now(), DefaultLocalCheckpointRefreshInterval,
	) {
		tempPath, err := p.produceCheckpoint(ctx)
		if err != nil {
			return err
		}
		defer removeSQLiteFiles(tempPath)

		p.files.mu.Lock()
		defer p.files.mu.Unlock()
		if err := p.installCheckpoint(tempPath); err != nil {
			return err
		}
		_, found, err = inspectBackupDatabase(
			ctx, p.files.checkpointPath, false, nil, nil)
		if err != nil {
			return fmt.Errorf("local backup producer: inspect produced checkpoint: %w", err)
		}
		if !found {
			return errors.New("local backup producer: produced checkpoint is unavailable")
		}
		return p.writeRestoreTest(ctx)
	}

	restored, found, err := inspectBackupDatabase(
		ctx, p.files.restoreTestPath, false, nil, nil)
	if err != nil {
		return fmt.Errorf("local backup producer: inspect restore test: %w", err)
	}
	if !found || restoredSnapshotDue(
		restored, checkpoint, p.now(), DefaultLocalRestoreTestRefreshInterval,
	) {
		p.files.mu.Lock()
		defer p.files.mu.Unlock()
		if err := p.writeRestoreTest(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Run maintains local backup evidence until ctx is canceled or maintenance
// fails. A failure is terminal so the daemon reports the broken safety gate.
func (p *LocalBackupProducer) Run(ctx context.Context) error {
	ticker := time.NewTicker(DefaultLocalBackupPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.Maintain(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func backupSnapshotDue(
	snapshot backupDatabaseSnapshot,
	current BackupHealthContext,
	now time.Time,
	refreshInterval time.Duration,
) bool {
	age := now.Sub(snapshot.generatedAt)
	return snapshot.schemaVersion != current.SchemaVersion ||
		snapshot.state.SyncEpoch != current.SyncEpoch ||
		snapshot.generatedAt.IsZero() ||
		age < 0 || age >= refreshInterval
}

func restoredSnapshotDue(
	restored backupDatabaseSnapshot,
	checkpoint backupDatabaseSnapshot,
	now time.Time,
	refreshInterval time.Duration,
) bool {
	age := now.Sub(restored.restoredAt)
	return restored.schemaVersion != checkpoint.schemaVersion ||
		restored.restoreCheckpointDigest != checkpoint.fileDigest ||
		age < 0 || age >= refreshInterval
}

func (p *LocalBackupProducer) produceCheckpoint(ctx context.Context) (string, error) {
	tempPath, err := unusedTemporaryPath(p.files.dir, ".latest-*.db")
	if err != nil {
		return "", fmt.Errorf("local backup producer: reserve checkpoint: %w", err)
	}
	success := false
	defer func() {
		if !success {
			removeSQLiteFiles(tempPath)
		}
	}()
	if err := p.store.Checkpoint(ctx, tempPath); err != nil {
		return "", fmt.Errorf("local backup producer: %w", err)
	}
	if err := writeLocalBackupCheckpointMarker(ctx, tempPath, p.now().UTC()); err != nil {
		return "", fmt.Errorf("local backup producer: record checkpoint time: %w", err)
	}
	success = true
	return tempPath, nil
}

func (p *LocalBackupProducer) installCheckpoint(tempPath string) error {
	if err := os.Rename(tempPath, p.files.checkpointPath); err != nil {
		return fmt.Errorf("local backup producer: install checkpoint: %w", err)
	}
	if err := syncLocalBackupDirectory(p.files.dir); err != nil {
		return fmt.Errorf("local backup producer: sync checkpoint directory: %w", err)
	}
	return nil
}

func writeLocalBackupCheckpointMarker(ctx context.Context, path string, generatedAt time.Time) error {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	_, writeErr := db.ExecContext(ctx,
		`INSERT INTO local_backup_checkpoint_marker (id, generated_at)
		 VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET generated_at = excluded.generated_at`,
		generatedAt.Format(time.RFC3339Nano))
	return errors.Join(writeErr, db.Close())
}

func (p *LocalBackupProducer) writeRestoreTest(ctx context.Context) error {
	checkpointDigest, err := localBackupFileDigest(p.files.checkpointPath)
	if err != nil {
		return fmt.Errorf("local backup producer: hash checkpoint for restore test: %w", err)
	}
	tempPath, err := unusedTemporaryPath(p.files.dir, ".restore-test-*.db")
	if err != nil {
		return fmt.Errorf("local backup producer: reserve restore test: %w", err)
	}
	defer removeSQLiteFiles(tempPath)

	restored, err := Open(ctx, tempPath, Options{})
	if err != nil {
		return fmt.Errorf("local backup producer: open restore test: %w", err)
	}
	_, restoreErr := restored.Restore(ctx, p.files.checkpointPath)
	var markerErr error
	if restoreErr == nil {
		_, markerErr = restored.db.ExecContext(ctx,
			`INSERT INTO local_backup_restore_marker
			    (id, checkpoint_digest, restored_at)
			 VALUES (1, ?, ?)
			 ON CONFLICT (id) DO UPDATE SET
			    checkpoint_digest = excluded.checkpoint_digest,
			    restored_at = excluded.restored_at`,
			checkpointDigest, p.now().UTC().Format(time.RFC3339Nano))
	}
	closeErr := restored.Close()
	if err := errors.Join(restoreErr, markerErr, closeErr); err != nil {
		return fmt.Errorf("local backup producer: restore test: %w", err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return fmt.Errorf("local backup producer: restrict restore test: %w", err)
	}
	if err := os.Rename(tempPath, p.files.restoreTestPath); err != nil {
		return fmt.Errorf("local backup producer: install restore test: %w", err)
	}
	if err := syncLocalBackupDirectory(p.files.dir); err != nil {
		return fmt.Errorf("local backup producer: sync restore-test directory: %w", err)
	}
	return nil
}

func defaultLocalBackupPaths(dbPath string) (dir, checkpointPath, restoreTestPath string, err error) {
	if dbPath == "" {
		return "", "", "", errors.New("local backup: empty database path")
	}
	dir = dbPath + ".checkpoints"
	return dir, filepath.Join(dir, "latest.db"), filepath.Join(dir, "restore-test.db"), nil
}

func ensurePrivateBackupDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s permissions are %04o, want owner-only", path, info.Mode().Perm())
	}
	return nil
}

func unusedTemporaryPath(dir, pattern string) (string, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := errors.Join(file.Close(), os.Remove(path)); err != nil {
		return "", err
	}
	return path, nil
}

func removeSQLiteFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-shm")
	_ = os.Remove(path + "-wal")
}

func syncLocalBackupDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // path is the validated local-backup directory
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
