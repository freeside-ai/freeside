package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// BackupArtifactStore is the part of the content-addressed blob store local
// backup health needs to prove closure.
type BackupArtifactStore interface {
	Verify(domain.Digest) (bool, error)
}

// BackupPayloadDigestExtractor validates one reconstructed durable task and
// returns every blob digest needed to replay it after restore.
type BackupPayloadDigestExtractor func(QueueEntry) ([]domain.Digest, error)

const (
	localBackupMarkerSchemaVersion = 15
	// DefaultLocalCheckpointMaxAge is the Phase 1A.2 currency window for the
	// local checkpoint. A daily checkpoint remains current across the writes
	// it protects while a missed daily cycle closes unattended admission.
	DefaultLocalCheckpointMaxAge = 24 * time.Hour
	// DefaultLocalRestoreTestMaxAge requires a successful restored copy at
	// least monthly. #238 can expose these policy values operationally without
	// changing the three health dimensions.
	DefaultLocalRestoreTestMaxAge = 30 * 24 * time.Hour
)

// LocalCheckpointHealthOptions names the owner-only local checkpoint, the
// restored copy that proves the checkpoint was tested, and the artifact store
// whose closure the checkpoint requires.
type LocalCheckpointHealthOptions struct {
	CheckpointPath    string
	RestoreTestPath   string
	Artifacts         BackupArtifactStore
	ApprovedRecipes   map[domain.Digest]bool
	PayloadExtractors map[string]BackupPayloadDigestExtractor
	CheckpointMaxAge  time.Duration
	RestoreTestMaxAge time.Duration
	Now               func() time.Time
}

// LocalBackupFiles owns the paired checkpoint and restore-test paths plus the
// in-process lease that keeps health evaluation on one installed generation.
type LocalBackupFiles struct {
	dir             string
	checkpointPath  string
	restoreTestPath string
	mu              sync.RWMutex
}

type localCheckpointHealthSource struct {
	files             *LocalBackupFiles
	artifacts         BackupArtifactStore
	approvedRecipes   map[domain.Digest]bool
	payloadExtractors map[string]BackupPayloadDigestExtractor
	checkpointMaxAge  time.Duration
	restoreTestMaxAge time.Duration
	now               func() time.Time
}

// NewDefaultLocalBackupFiles uses the daemon's established owner-only
// checkpoint directory beside the database.
func NewDefaultLocalBackupFiles(dbPath string) (*LocalBackupFiles, error) {
	dir, checkpointPath, restoreTestPath, err := defaultLocalBackupPaths(dbPath)
	if err != nil {
		return nil, fmt.Errorf("local backup files: %w", err)
	}
	return &LocalBackupFiles{
		dir: dir, checkpointPath: checkpointPath, restoreTestPath: restoreTestPath,
	}, nil
}

// NewCheckpointHealthSource builds the health evaluator paired with this
// file set. The producer installs latest.db and restore-test.db under the same
// lease that this source holds while inspecting both.
func (f *LocalBackupFiles) NewCheckpointHealthSource(
	artifacts BackupArtifactStore,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (BackupHealthSource, error) {
	return newLocalCheckpointHealthSource(LocalCheckpointHealthOptions{
		Artifacts:         artifacts,
		ApprovedRecipes:   approvedRecipes,
		PayloadExtractors: payloadExtractors,
	}, f)
}

// NewLocalCheckpointHealthSource builds the provisional Phase 1A.2 evaluator.
// The paths are an implementation detail behind BackupHealthSource; #305 can
// replace it with the encrypted checkpoint without changing admission.
func NewLocalCheckpointHealthSource(opts LocalCheckpointHealthOptions) (BackupHealthSource, error) {
	return newLocalCheckpointHealthSource(opts, &LocalBackupFiles{
		checkpointPath:  opts.CheckpointPath,
		restoreTestPath: opts.RestoreTestPath,
	})
}

func newLocalCheckpointHealthSource(
	opts LocalCheckpointHealthOptions, files *LocalBackupFiles,
) (BackupHealthSource, error) {
	if opts.CheckpointMaxAge == 0 {
		opts.CheckpointMaxAge = DefaultLocalCheckpointMaxAge
	}
	if opts.RestoreTestMaxAge == 0 {
		opts.RestoreTestMaxAge = DefaultLocalRestoreTestMaxAge
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	switch {
	case files == nil:
		return nil, errors.New("local checkpoint health: nil backup files")
	case files.checkpointPath == "":
		return nil, errors.New("local checkpoint health: empty checkpoint path")
	case files.restoreTestPath == "":
		return nil, errors.New("local checkpoint health: empty restore-test path")
	case opts.Artifacts == nil:
		return nil, errors.New("local checkpoint health: nil artifact store")
	case opts.CheckpointMaxAge < 0:
		return nil, errors.New("local checkpoint health: negative checkpoint max age")
	case opts.RestoreTestMaxAge < 0:
		return nil, errors.New("local checkpoint health: negative restore-test max age")
	}
	return &localCheckpointHealthSource{
		files:             files,
		artifacts:         opts.Artifacts,
		approvedRecipes:   maps.Clone(opts.ApprovedRecipes),
		payloadExtractors: maps.Clone(opts.PayloadExtractors),
		checkpointMaxAge:  opts.CheckpointMaxAge,
		restoreTestMaxAge: opts.RestoreTestMaxAge,
		now:               opts.Now,
	}, nil
}

type backupDatabaseSnapshot struct {
	state                   ServerState
	schemaVersion           int
	digests                 []domain.Digest
	fileDigest              domain.Digest
	restoreCheckpointDigest domain.Digest
	generatedAt             time.Time
	restoredAt              time.Time
}

func unhealthyBackupHealth() domain.BackupHealth {
	return domain.BackupHealth{
		CheckpointCurrency: domain.BackupHealthUnhealthy,
		ArtifactClosure:    domain.BackupHealthUnhealthy,
		RestoreTestAge:     domain.BackupHealthUnhealthy,
	}
}

func (s *localCheckpointHealthSource) BackupHealth(
	ctx context.Context, current BackupHealthContext,
) (domain.BackupHealth, error) {
	s.files.mu.RLock()
	defer s.files.mu.RUnlock()

	health := unhealthyBackupHealth()
	checkpoint, found, err := inspectBackupDatabase(
		ctx, s.files.checkpointPath, true, s.approvedRecipes, s.payloadExtractors)
	if err != nil {
		return domain.BackupHealth{}, fmt.Errorf("local checkpoint health: %w", err)
	}
	if !found {
		return health, nil
	}
	now := s.now()
	checkpointAge := now.Sub(checkpoint.generatedAt)
	if checkpoint.schemaVersion == current.SchemaVersion &&
		checkpoint.state.SyncEpoch == current.SyncEpoch &&
		!checkpoint.generatedAt.IsZero() &&
		checkpointAge >= 0 && checkpointAge <= s.checkpointMaxAge {
		health.CheckpointCurrency = domain.BackupHealthHealthy
	}

	closed := true
	for _, digest := range checkpoint.digests {
		verified, err := s.artifacts.Verify(digest)
		if err != nil {
			return domain.BackupHealth{}, fmt.Errorf("local checkpoint health: artifact %s: %w", digest, err)
		}
		if !verified {
			closed = false
		}
	}
	if closed {
		health.ArtifactClosure = domain.BackupHealthHealthy
	}

	restored, found, err := inspectBackupDatabase(
		ctx, s.files.restoreTestPath, false, nil, nil)
	if err != nil {
		return domain.BackupHealth{}, fmt.Errorf("local checkpoint health: restore test: %w", err)
	}
	if found {
		restoreTestAge := now.Sub(restored.restoredAt)
		if restoreTestAge >= 0 && restoreTestAge <= s.restoreTestMaxAge &&
			restored.schemaVersion == checkpoint.schemaVersion &&
			restored.restoreCheckpointDigest == checkpoint.fileDigest {
			health.RestoreTestAge = domain.BackupHealthHealthy
		}
	}
	return health, nil
}

func inspectBackupDatabase(
	ctx context.Context, path string, collectDigests bool,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) (backupDatabaseSnapshot, bool, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return backupDatabaseSnapshot{}, false, nil
	case err != nil:
		return backupDatabaseSnapshot{}, false, fmt.Errorf("stat %s: %w", path, err)
	case !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0:
		return backupDatabaseSnapshot{}, false, nil
	}

	q := url.Values{"mode": []string{"ro"}}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return backupDatabaseSnapshot{}, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	fileDigest, err := localBackupFileDigest(path)
	if err != nil {
		return backupDatabaseSnapshot{}, false, fmt.Errorf("hash %s: %w", path, err)
	}
	snapshot := backupDatabaseSnapshot{fileDigest: fileDigest}
	if err := db.QueryRowContext(ctx,
		`SELECT sync_epoch, revision FROM server_state WHERE id = 1`).
		Scan(&snapshot.state.SyncEpoch, &snapshot.state.Revision); err != nil {
		return backupDatabaseSnapshot{}, false, fmt.Errorf("read %s server state: %w", path, err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).
		Scan(&snapshot.schemaVersion); err != nil {
		return backupDatabaseSnapshot{}, false, fmt.Errorf("read %s schema version: %w", path, err)
	}
	if snapshot.schemaVersion >= localBackupMarkerSchemaVersion {
		var generatedAt string
		err = db.QueryRowContext(ctx,
			`SELECT generated_at
			   FROM local_backup_checkpoint_marker WHERE id = 1`).
			Scan(&generatedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return backupDatabaseSnapshot{}, false,
				fmt.Errorf("read %s checkpoint marker: %w", path, err)
		default:
			snapshot.generatedAt, err = time.Parse(time.RFC3339Nano, generatedAt)
			if err != nil {
				return backupDatabaseSnapshot{}, false,
					fmt.Errorf("read %s checkpoint marker time: %w", path, err)
			}
		}

		var restoredAt string
		err = db.QueryRowContext(ctx,
			`SELECT checkpoint_digest, restored_at
			   FROM local_backup_restore_marker WHERE id = 1`).
			Scan(&snapshot.restoreCheckpointDigest, &restoredAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return backupDatabaseSnapshot{}, false, fmt.Errorf("read %s restore marker: %w", path, err)
		default:
			snapshot.restoredAt, err = time.Parse(time.RFC3339Nano, restoredAt)
			if err != nil {
				return backupDatabaseSnapshot{}, false,
					fmt.Errorf("read %s restore marker time: %w", path, err)
			}
		}
	}
	if collectDigests {
		digests, err := checkpointArtifactDigests(
			ctx, db, approvedRecipes, payloadExtractors)
		if err != nil {
			return backupDatabaseSnapshot{}, false, fmt.Errorf("read %s artifact closure: %w", path, err)
		}
		snapshot.digests = digests
	}
	return snapshot, true, nil
}

func checkpointArtifactDigests(
	ctx context.Context,
	db *sql.DB,
	approvedRecipes map[domain.Digest]bool,
	payloadExtractors map[string]BackupPayloadDigestExtractor,
) ([]domain.Digest, error) {
	digests := make(map[domain.Digest]struct{})
	sqlTx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlTx.Rollback() }()
	readTx := &ReadTx{tx: sqlTx, approvedRecipes: approvedRecipes}

	artifactIDs, err := checkpointIDs[domain.ArtifactID](
		ctx, sqlTx, `SELECT id FROM artifacts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for _, id := range artifactIDs {
		artifact, err := readTx.GetArtifact(ctx, id)
		if err != nil {
			return nil, err
		}
		digests[artifact.Digest] = struct{}{}
	}

	conversations, err := readTx.ListConversations(ctx)
	if err != nil {
		return nil, err
	}
	for _, snapshotted := range conversations {
		conversation := snapshotted.Value
		for _, message := range conversation.Messages {
			for _, digest := range message.Attachments {
				digests[digest] = struct{}{}
			}
		}
	}

	items, err := readTx.ListAttentionItems(ctx)
	if err != nil {
		return nil, err
	}
	for _, snapshotted := range items {
		item := snapshotted.Value
		for _, artifact := range item.EvidenceSnapshot {
			digests[artifact.Digest] = struct{}{}
		}
		for _, claim := range item.AgentClaims {
			if claim.Text == nil {
				digests[claim.Digest] = struct{}{}
			}
		}
	}

	commandIDs, err := checkpointIDs[string](
		ctx, sqlTx, `SELECT command_id FROM commands ORDER BY command_id`)
	if err != nil {
		return nil, err
	}
	for _, commandID := range commandIDs {
		command, inline, _, err := readTx.getStoredCommandSnapshot(ctx, commandID)
		if err != nil {
			return nil, err
		}
		for _, digest := range command.ArtifactDigests {
			if _, carriedInline := inline[digest]; !carriedInline {
				digests[digest] = struct{}{}
			}
		}
		for _, digest := range command.Attachments {
			digests[digest] = struct{}{}
		}
	}

	outboxKeys, err := checkpointIDs[string](
		ctx, sqlTx, `SELECT idempotency_key FROM outbox ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for _, key := range outboxKeys {
		entry, err := readTx.GetOutbox(ctx, key)
		if err != nil {
			return nil, err
		}
		extract := payloadExtractors[entry.Kind]
		if extract == nil {
			return nil, fmt.Errorf(
				"outbox %q backup references: unregistered kind %q", key, entry.Kind)
		}
		references, err := extract(entry)
		if err != nil {
			return nil, fmt.Errorf("outbox %q backup references: %w", key, err)
		}
		for _, digest := range references {
			if digest == "" {
				return nil, fmt.Errorf("outbox %q backup references: empty digest", key)
			}
			digests[digest] = struct{}{}
		}
	}

	out := make([]domain.Digest, 0, len(digests))
	for digest := range digests {
		out = append(out, digest)
	}
	return out, nil
}

func checkpointIDs[T ~string](
	ctx context.Context, tx *sql.Tx, query string,
) ([]T, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []T{}
	for rows.Next() {
		var id T
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func localBackupFileDigest(path string) (domain.Digest, error) {
	file, err := os.Open(path) //nolint:gosec // validated owner-only backup path
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return "", err
	}
	return domain.Digest(fmt.Sprintf("sha256:%x", hash.Sum(nil))), nil
}
