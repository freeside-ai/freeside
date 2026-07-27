package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type blockingBackupArtifactStore struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
}

func (s *blockingBackupArtifactStore) Verify(domain.Digest) (bool, error) {
	s.enteredOnce.Do(func() { close(s.entered) })
	<-s.release
	return true, nil
}

func TestLocalBackupProducerCreatesRefreshesAndVerifiesEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	blobPath := dbPath + ".blobs"
	blobs, err := signet.NewBlobStore(blobPath)
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(blobs, approvedFixtureRecipes(), nil)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{
		ApprovedRecipes: approvedFixtureRecipes(), BackupHealthSource: source,
	})

	body := "checkpointed artifact"
	digest := domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))))
	if _, err := blobs.Put(digest, strings.NewReader(body)); err != nil {
		t.Fatalf("Put blob: %v", err)
	}
	fixtures := newFixtures(t)
	artifact := fixtures.artifact
	artifact.Digest = digest
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutArtifact(ctx, artifact)
	}); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}

	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	assertHealthyLocalBackup(t, s)

	checkpointDir := dbPath + ".checkpoints"
	checkpointPath := filepath.Join(checkpointDir, "latest.db")
	restoreTestPath := filepath.Join(checkpointDir, "restore-test.db")
	for path, wantMode := range map[string]os.FileMode{
		checkpointDir: 0o700, checkpointPath: 0o600, restoreTestPath: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s mode = %04o, want %04o", path, got, wantMode)
		}
	}

	oldCheckpoint := time.Now().Add(-store.DefaultLocalCheckpointRefreshInterval - time.Hour)
	oldRestoreTest := time.Now().Add(-store.DefaultLocalRestoreTestRefreshInterval - time.Hour)
	writeCheckpointGeneratedAt(t, checkpointPath, oldCheckpoint)
	touchedAt := time.Now()
	if err := os.Chtimes(checkpointPath, touchedAt, touchedAt); err != nil {
		t.Fatalf("touch old checkpoint: %v", err)
	}
	if err := os.Chtimes(restoreTestPath, oldRestoreTest, oldRestoreTest); err != nil {
		t.Fatalf("age restore test: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain stale evidence: %v", err)
	}
	assertHealthyLocalBackup(t, s)

	contentPath := filepath.Join(blobPath, "sha256-"+strings.TrimPrefix(string(digest), "sha256:"))
	if err := os.WriteFile(contentPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	health, err := s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth with corrupted blob: %v", err)
	}
	if health.ArtifactClosure != domain.BackupHealthUnhealthy {
		t.Fatalf("corrupted blob closure = %q, want unhealthy", health.ArtifactClosure)
	}
}

func TestLocalBackupProducerRejectsWidenedDirectory(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	checkpointDir := dbPath + ".checkpoints"
	if err := os.Mkdir(checkpointDir, 0o700); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	if err := os.Chmod(checkpointDir, 0o755); err != nil { //nolint:gosec // deliberately widened adversarial fixture
		t.Fatalf("widen checkpoint directory: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{})
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(context.Background()); err == nil {
		t.Fatal("Maintain accepted a group/world-readable checkpoint directory")
	}
	if _, err := os.Stat(filepath.Join(checkpointDir, "latest.db")); !os.IsNotExist(err) {
		t.Fatalf("rejected directory produced a checkpoint: %v", err)
	}
}

func TestLocalBackupHealthPinsCheckpointAndRestoreGeneration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	artifacts := &blockingBackupArtifactStore{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	source, err := files.NewCheckpointHealthSource(
		artifacts, approvedFixtureRecipes(), nil)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{
		ApprovedRecipes: approvedFixtureRecipes(), BackupHealthSource: source,
	})
	fixture := newFixtures(t).artifact
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutArtifact(ctx, fixture)
	}); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("initial Maintain: %v", err)
	}

	checkpointDir := dbPath + ".checkpoints"
	checkpointPath := filepath.Join(checkpointDir, "latest.db")
	writeCheckpointGeneratedAt(
		t, checkpointPath, time.Now().Add(-store.DefaultLocalCheckpointRefreshInterval-time.Hour))
	before, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned checkpoint path
	if err != nil {
		t.Fatalf("read checkpoint before concurrent refresh: %v", err)
	}

	healthDone := make(chan error, 1)
	go func() {
		_, err := source.BackupHealth(ctx, store.BackupHealthContext{})
		healthDone <- err
	}()
	<-artifacts.entered

	maintainDone := make(chan error, 1)
	go func() { maintainDone <- producer.Maintain(ctx) }()
	waitForLocalBackupTemp(t, checkpointDir, ".latest-")

	during, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned checkpoint path
	if err != nil {
		t.Fatalf("read checkpoint during concurrent refresh: %v", err)
	}
	if !bytes.Equal(during, before) {
		t.Fatal("producer replaced checkpoint while health evaluation held its generation lease")
	}

	close(artifacts.release)
	if err := <-healthDone; err != nil {
		t.Fatalf("concurrent BackupHealth: %v", err)
	}
	if err := <-maintainDone; err != nil {
		t.Fatalf("concurrent Maintain: %v", err)
	}
	assertHealthyLocalBackup(t, s)
}

func waitForLocalBackupTemp(t *testing.T, dir, prefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read local backup directory: %v", err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for local backup temporary file %q", prefix)
}

func assertHealthyLocalBackup(t *testing.T, s *store.Store) {
	t.Helper()
	health, err := s.BackupHealth(context.Background())
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	want := healthyBackupHealth()
	if health != want {
		t.Fatalf("BackupHealth = %+v, want %+v", health, want)
	}
}
