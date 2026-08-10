package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
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

// LocalBackupProducer maintains the encrypted, digest-bound owner-only
// checkpoint and its authenticated restore-test timestamp.
type LocalBackupProducer struct {
	store *Store
	files *LocalBackupFiles
	now   func() time.Time
	mu    sync.Mutex
}

// NewProducer builds the producer paired with this file set's health source.
// From here until its first pass, the artifact closure reports unhealthy: this
// file set now has a live database behind it, and nothing has scanned it yet,
// so the checkpoint's own verdict would answer a question about the store it
// cannot see. That keeps the refusal from depending on where in a startup
// sequence the first pass happens to sit.
func (f *LocalBackupFiles) NewProducer(store *Store) (*LocalBackupProducer, error) {
	if store == nil {
		return nil, errors.New("local backup producer: nil store")
	}
	if f == nil {
		return nil, errors.New("local backup producer: nil backup files")
	}
	f.liveClosureGap.Store(true)
	return &LocalBackupProducer{
		store: store, files: f, now: time.Now,
	}, nil
}

// Maintain refreshes evidence that is missing, incompatible with the live
// store, or beyond its safety-margin interval. Every successful pass also
// reasserts the absence of legacy plaintext backup files.
func (p *LocalBackupProducer) Maintain(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := ensurePrivateBackupDirectory(p.files.dir); err != nil {
		return fmt.Errorf("local backup producer: %w", err)
	}
	if err := removeStaleCheckpointTemps(p.files.dir); err != nil {
		return fmt.Errorf("local backup producer: %w", err)
	}
	var (
		current BackupHealthContext
		live    artifactClosure
	)
	if err := p.store.Read(ctx, func(tx *ReadTx) error {
		var err error
		if current, err = tx.backupHealthContext(ctx); err != nil {
			return err
		}
		live, err = outboxArtifactDigests(ctx, tx, p.files.payloadExtractors)
		return err
	}); err != nil {
		return fmt.Errorf("local backup producer: read live state: %w", err)
	}
	// The live database, not the last checkpoint, is what a checkpoint has to
	// describe, so the verdict is refreshed every pass: it closes unattended
	// admission while the gap holds and reopens it once the row reads again,
	// without waiting for the current checkpoint to age out.
	p.files.liveClosureGap.Store(live.gap != nil)
	if live.gap != nil {
		// Producing here would seal a manifest that omits references the
		// checkpoint must assert, so the pass ends without one; see
		// ErrBackupClosureIncomplete for why that is reported, not fatal.
		// Legacy plaintext cleanup stays behind the same deletion-before-proof
		// rule every other failed pass follows: this pass proved no
		// checkpoint, so it must not remove the last fallback under one.
		return fmt.Errorf("local backup producer: %w: %w",
			ErrBackupClosureIncomplete, live.gap)
	}

	checkpoint, metadata, found, err := inspectEncryptedCheckpoint(
		ctx,
		current.SchemaVersion,
		p.files,
		true,
		p.files.approvedRecipes,
		p.files.payloadExtractors,
	)
	// A stored checkpoint this binary cannot verify is unusable, not fatal:
	// a stale schema, a manifest that no longer matches current policy, and a
	// closure this binary cannot recompute are replaced by production below.
	if errors.Is(err, errCheckpointManifestMismatch) ||
		errors.Is(err, ErrBackupClosureIncomplete) ||
		errors.Is(err, errCheckpointSchemaStale) {
		found, err = false, nil
	}
	if err != nil {
		return fmt.Errorf("local backup producer: inspect checkpoint: %w", err)
	}
	if !found || backupSnapshotDue(
		checkpoint, current, p.now(), DefaultLocalCheckpointRefreshInterval,
	) {
		plaintext, next, err := p.produceCheckpoint(ctx)
		if err != nil {
			return err
		}
		body, err := sealEncryptedCheckpoint(plaintext, next, p.files.encryptionKey)
		if err != nil {
			return fmt.Errorf("local backup producer: %w", err)
		}

		p.files.mu.Lock()
		defer p.files.mu.Unlock()
		if err := installEncryptedCheckpoint(
			p.files.dir, p.files.checkpointPath, body,
		); err != nil {
			return err
		}
		_, _, found, err = inspectEncryptedCheckpoint(
			ctx, current.SchemaVersion, p.files, true,
			p.files.approvedRecipes, p.files.payloadExtractors)
		if err != nil {
			return fmt.Errorf("local backup producer: inspect produced checkpoint: %w", err)
		}
		if !found {
			return errors.New("local backup producer: produced checkpoint is unavailable")
		}
		if err := p.writeRestoreTest(ctx); err != nil {
			return err
		}
	} else if restoredSnapshotDue(metadata, p.now(), DefaultLocalRestoreTestRefreshInterval) {
		p.files.mu.Lock()
		defer p.files.mu.Unlock()
		if err := p.writeRestoreTest(ctx); err != nil {
			return err
		}
	}
	return removeLegacyLocalBackupFiles(p.files)
}

// Run maintains local backup evidence until ctx is canceled or maintenance
// fails. A failure is terminal so the daemon reports the broken safety gate,
// with one exception: an artifact closure this binary cannot compute is a
// reported state, not a failed pass, and the loop keeps running so a later
// pass recovers once the row reads again (see ErrBackupClosureIncomplete).
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
				if errors.Is(err, ErrBackupClosureIncomplete) {
					continue
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
	checkpoint domain.BackupCheckpoint,
	now time.Time,
	refreshInterval time.Duration,
) bool {
	if checkpoint.RestoreTestedAt == nil {
		return true
	}
	age := now.Sub(*checkpoint.RestoreTestedAt)
	return age < 0 || age >= refreshInterval
}

func (p *LocalBackupProducer) produceCheckpoint(
	ctx context.Context,
) ([]byte, domain.BackupCheckpoint, error) {
	plaintext, err := serializeStoreCheckpoint(ctx, p.store)
	if err != nil {
		return nil, domain.BackupCheckpoint{},
			fmt.Errorf("local backup producer: %w", err)
	}
	db, conn, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		return nil, domain.BackupCheckpoint{}, err
	}
	defer closeDeserializedBackupDatabase(db, conn)
	snapshot, err := inspectBackupDB(
		ctx,
		conn,
		digestBytes(plaintext),
		true,
		p.files.approvedRecipes,
		p.files.payloadExtractors,
	)
	if err != nil {
		return nil, domain.BackupCheckpoint{},
			fmt.Errorf("local backup producer: inspect new checkpoint: %w", err)
	}
	if snapshot.closureGap != nil {
		// Maintain refuses the gap before reaching here from the live
		// database; this is the seal-time guard, so no manifest can ever be
		// computed from a scan that describes less than its snapshot holds.
		// Recording it keeps health closed if the two scans ever disagree.
		p.files.liveClosureGap.Store(true)
		return nil, domain.BackupCheckpoint{}, fmt.Errorf(
			"local backup producer: seal checkpoint: %w: %w",
			ErrBackupClosureIncomplete, snapshot.closureGap)
	}
	checkpointID, err := randomEpoch()
	if err != nil {
		return nil, domain.BackupCheckpoint{},
			fmt.Errorf("local backup producer: generate checkpoint id: %w", err)
	}
	createdAt := p.now().UTC()
	checkpoint := domain.BackupCheckpoint{
		CheckpointID:           checkpointID,
		SyncEpoch:              snapshot.state.SyncEpoch,
		ServerRevision:         snapshot.state.Revision,
		SQLiteSnapshotDigest:   snapshot.fileDigest,
		ArtifactManifestDigest: artifactManifestDigest(snapshot.digests),
		CreatedAt:              createdAt,
		CompletedAt:            createdAt,
	}
	if err := checkpoint.Validate(); err != nil {
		return nil, domain.BackupCheckpoint{},
			fmt.Errorf("local backup producer: checkpoint metadata: %w", err)
	}
	return plaintext, checkpoint, nil
}

func (p *LocalBackupProducer) writeRestoreTest(ctx context.Context) error {
	plaintext, checkpoint, err := openEncryptedCheckpoint(
		p.files.checkpointPath, p.files.encryptionKey)
	if err != nil {
		return fmt.Errorf("local backup producer: decrypt restore test: %w", err)
	}
	sourceDB, source, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		return fmt.Errorf("local backup producer: open restore source: %w", err)
	}
	defer closeDeserializedBackupDatabase(sourceDB, source)

	restored, err := Open(ctx, ":memory:", Options{
		ApprovedRecipes: p.files.approvedRecipes,
	})
	if err != nil {
		return fmt.Errorf("local backup producer: open restore test: %w", err)
	}
	state, restoreErr := restored.restoreFromDatabase(ctx, source)
	var verifyErr error
	if restoreErr == nil && state.Revision != checkpoint.ServerRevision {
		verifyErr = fmt.Errorf(
			"restored revision %d does not match checkpoint revision %d",
			state.Revision, checkpoint.ServerRevision)
	}
	closeErr := restored.Close()
	if err := errors.Join(restoreErr, verifyErr, closeErr); err != nil {
		return fmt.Errorf("local backup producer: restore test: %w", err)
	}
	restoreTestedAt := p.now().UTC()
	checkpoint.RestoreTestedAt = &restoreTestedAt
	body, err := sealEncryptedCheckpoint(plaintext, checkpoint, p.files.encryptionKey)
	if err != nil {
		return fmt.Errorf("local backup producer: record restore test: %w", err)
	}
	return installEncryptedCheckpoint(p.files.dir, p.files.checkpointPath, body)
}

func defaultLocalBackupPaths(dbPath string) (dir, checkpointPath, restoreTestPath string, err error) {
	if dbPath == "" {
		return "", "", "", errors.New("local backup: empty database path")
	}
	dir = dbPath + ".checkpoints"
	return dir,
		filepath.Join(dir, encryptedCheckpointFilename),
		filepath.Join(dir, legacyRestoreTestFilename),
		nil
}

func removeLegacyLocalBackupFiles(files *LocalBackupFiles) error {
	for _, path := range []string{
		filepath.Join(files.dir, legacyCheckpointFilename),
		files.restoreTestPath,
	} {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("local backup producer: remove legacy plaintext %s: %w",
					path+suffix, err)
			}
		}
	}
	return atomicfile.SyncDir(files.dir)
}

func removeStaleCheckpointTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read checkpoint directory for stale temporary files: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		legacySQLiteTemp := (strings.HasPrefix(name, ".latest-") ||
			strings.HasPrefix(name, ".restore-test-")) &&
			(strings.HasSuffix(name, ".db") ||
				strings.HasSuffix(name, ".db-wal") ||
				strings.HasSuffix(name, ".db-shm"))
		encryptedCheckpointTemp := strings.HasPrefix(name, ".latest-") &&
			strings.HasSuffix(name, ".backup")
		atomicCheckpointTemp := strings.HasPrefix(name, "."+encryptedCheckpointFilename+"-") &&
			strings.HasSuffix(name, ".tmp")
		if !legacySQLiteTemp && !encryptedCheckpointTemp && !atomicCheckpointTemp {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat stale checkpoint temporary %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("stale checkpoint temporary %s is not a regular file", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale checkpoint temporary %s: %w", path, err)
		}
	}
	return nil
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
