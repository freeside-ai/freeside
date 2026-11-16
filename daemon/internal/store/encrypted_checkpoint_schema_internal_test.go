package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
