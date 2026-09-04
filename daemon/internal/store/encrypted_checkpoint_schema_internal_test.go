package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

type schemaCheckpointArtifacts struct{}

func (schemaCheckpointArtifacts) Verify(domain.Digest) (bool, error) { return true, nil }

type schemaCheckpointTestStore struct {
	ctx      context.Context
	files    *LocalBackupFiles
	source   BackupHealthSource
	live     *Store
	producer *LocalBackupProducer
}

func TestLocalBackupProducerReplacesOlderSchemaCheckpoint(t *testing.T) {
	t.Parallel()
	fixture := newSchemaCheckpointTestStore(t)
	current := schemaTestBackupHealthContext(t, fixture.ctx, fixture.live)
	const olderVersion = 34 // 0035 added the health_posture column read by the current closure scan.
	if current.SchemaVersion <= olderVersion {
		t.Fatalf("current schema version = %d, need newer than %d",
			current.SchemaVersion, olderVersion)
	}
	installSchemaCheckpointFixture(t, fixture.ctx, fixture.files, olderVersion)
	staleBody, err := os.ReadFile(fixture.files.checkpointPath)
	if err != nil {
		t.Fatalf("read older-schema checkpoint: %v", err)
	}

	health, err := fixture.source.BackupHealth(fixture.ctx, current)
	if err != nil {
		t.Fatalf("BackupHealth over older schema: %v", err)
	}
	if health != unhealthyBackupHealth() {
		t.Fatalf("BackupHealth over older schema = %+v, want %+v",
			health, unhealthyBackupHealth())
	}

	if err := fixture.producer.Maintain(fixture.ctx); err != nil {
		t.Fatalf("Maintain over older schema: %v", err)
	}
	refreshedBody, err := os.ReadFile(fixture.files.checkpointPath)
	if err != nil {
		t.Fatalf("read refreshed checkpoint: %v", err)
	}
	if bytes.Equal(refreshedBody, staleBody) {
		t.Fatal("maintenance retained the older-schema checkpoint")
	}
	snapshot, _, found, err := inspectEncryptedCheckpoint(
		fixture.ctx, current.SchemaVersion, fixture.files, true, nil, nil)
	if err != nil {
		t.Fatalf("inspect refreshed checkpoint: %v", err)
	}
	if !found {
		t.Fatal("refreshed checkpoint is unavailable")
	}
	if snapshot.schemaVersion != current.SchemaVersion {
		t.Fatalf("refreshed schema version = %d, want %d",
			snapshot.schemaVersion, current.SchemaVersion)
	}
}

func TestLocalBackupProducerKeepsSameSchemaClosureFailureFatal(t *testing.T) {
	t.Parallel()
	fixture := newSchemaCheckpointTestStore(t)
	if err := fixture.producer.Maintain(fixture.ctx); err != nil {
		t.Fatalf("initial Maintain: %v", err)
	}
	if err := MutateEncryptedCheckpointForTest(
		fixture.ctx, fixture.files,
		`ALTER TABLE attention_items RENAME TO attention_items_unreadable`,
	); err != nil {
		t.Fatalf("make same-schema closure unreadable: %v", err)
	}
	before, err := os.ReadFile(fixture.files.checkpointPath)
	if err != nil {
		t.Fatalf("read unreadable checkpoint: %v", err)
	}

	err = fixture.producer.Maintain(fixture.ctx)
	if err == nil || !strings.Contains(err.Error(), "no such table: attention_items") {
		t.Fatalf("Maintain over same-schema closure = %v, want missing-table failure", err)
	}
	if errors.Is(err, errCheckpointSchemaStale) {
		t.Fatalf("same-schema closure failure was classified stale: %v", err)
	}
	after, err := os.ReadFile(fixture.files.checkpointPath)
	if err != nil {
		t.Fatalf("read retained checkpoint: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("same-schema closure failure replaced the checkpoint")
	}
}

func TestEncryptedCheckpointRejectsNewerSchemaExplicitly(t *testing.T) {
	t.Parallel()
	fixture := newSchemaCheckpointTestStore(t)
	if err := fixture.producer.Maintain(fixture.ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	current := schemaTestBackupHealthContext(t, fixture.ctx, fixture.live)
	expectedVersion := current.SchemaVersion - 1
	before, err := os.ReadFile(fixture.files.checkpointPath)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}

	_, _, _, err = inspectEncryptedCheckpoint(
		fixture.ctx, expectedVersion, fixture.files, true, nil, nil)
	want := fmt.Sprintf("checkpoint schema version %d is newer than binary version %d",
		current.SchemaVersion, expectedVersion)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("inspect newer schema = %v, want %q", err, want)
	}
	if errors.Is(err, errCheckpointSchemaStale) {
		t.Fatalf("newer schema was classified stale: %v", err)
	}
	after, err := os.ReadFile(fixture.files.checkpointPath)
	if err != nil {
		t.Fatalf("read checkpoint after rejection: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("newer-schema inspection changed the checkpoint")
	}
}

func newSchemaCheckpointTestStore(t *testing.T) schemaCheckpointTestStore {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	files, err := NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(schemaCheckpointArtifacts{}, nil, nil)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	live := openTemplateStoreAt(t, dbPath, Options{BackupHealthSource: source})
	t.Cleanup(func() {
		if err := live.Close(); err != nil {
			t.Errorf("close live store: %v", err)
		}
	})
	producer, err := files.NewProducer(live)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	return schemaCheckpointTestStore{
		ctx: ctx, files: files, source: source, live: live, producer: producer,
	}
}

func migrationsBeforeVersion(t *testing.T, threshold int) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	prefix := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		versionText, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			t.Fatalf("migration %q has no version prefix", entry.Name())
		}
		version, err := strconv.Atoi(versionText)
		if err != nil {
			t.Fatalf("parse migration version %q: %v", entry.Name(), err)
		}
		if version >= threshold {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, entry.Name())
		if err != nil {
			t.Fatalf("read migration %q: %v", entry.Name(), err)
		}
		prefix[entry.Name()] = &fstest.MapFile{Data: body}
	}
	return prefix
}

func installSchemaCheckpointFixture(
	t *testing.T, ctx context.Context, files *LocalBackupFiles, schemaVersion int,
) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "checkpoint.db"), Options{})
	if err != nil {
		t.Fatalf("open checkpoint database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close checkpoint database: %v", err)
		}
	})
	if err := migrate(ctx, db, migrationsBeforeVersion(t, schemaVersion+1)); err != nil {
		t.Fatalf("migrate checkpoint database: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seed checkpoint epoch: %v", err)
	}
	plaintext, err := serializeStoreCheckpoint(ctx, &Store{db: db})
	if err != nil {
		t.Fatalf("serialize checkpoint database: %v", err)
	}
	snapshot, err := inspectBackupDB(ctx, db, digestBytes(plaintext), false, nil, nil)
	if err != nil {
		t.Fatalf("inspect checkpoint database metadata: %v", err)
	}
	if snapshot.schemaVersion != schemaVersion {
		t.Fatalf("fixture schema version = %d, want %d", snapshot.schemaVersion, schemaVersion)
	}
	now := time.Now().UTC()
	checkpoint := domain.BackupCheckpoint{
		CheckpointID:           "older-schema-checkpoint",
		SyncEpoch:              snapshot.state.SyncEpoch,
		ServerRevision:         snapshot.state.Revision,
		SQLiteSnapshotDigest:   snapshot.fileDigest,
		ArtifactManifestDigest: artifactManifestDigest(nil),
		CreatedAt:              now,
		CompletedAt:            now,
	}
	body, err := sealEncryptedCheckpoint(plaintext, checkpoint, files.encryptionKey)
	if err != nil {
		t.Fatalf("seal schema checkpoint: %v", err)
	}
	if err := os.MkdirAll(files.dir, 0o700); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	if err := installEncryptedCheckpoint(files.dir, files.checkpointPath, body); err != nil {
		t.Fatalf("install schema checkpoint: %v", err)
	}
}

func schemaTestBackupHealthContext(
	t *testing.T, ctx context.Context, s *Store,
) BackupHealthContext {
	t.Helper()
	state, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	schemaVersion, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	return BackupHealthContext{ServerState: state, SchemaVersion: schemaVersion}
}
