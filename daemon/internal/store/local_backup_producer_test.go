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
	checkpointDir := dbPath + ".checkpoints"
	if err := os.Mkdir(checkpointDir, 0o700); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	legacyNames := []string{
		"latest.db", "latest.db-wal", "latest.db-shm",
		"restore-test.db", "restore-test.db-wal", "restore-test.db-shm",
		".latest-stale.db", ".latest-stale.db-wal", ".latest-stale.db-shm",
		".latest-stale.backup",
		".restore-test-stale.db", ".restore-test-stale.db-wal",
		".restore-test-stale.db-shm",
	}
	seedLegacyPlaintext := func() {
		t.Helper()
		for _, name := range legacyNames {
			if err := os.WriteFile(
				filepath.Join(checkpointDir, name), []byte("legacy credential-bearing bytes"), 0o600,
			); err != nil {
				t.Fatalf("seed legacy backup %s: %v", name, err)
			}
		}
	}
	assertLegacyPlaintextRemoved := func() {
		t.Helper()
		for _, name := range legacyNames {
			if _, err := os.Lstat(filepath.Join(checkpointDir, name)); !os.IsNotExist(err) {
				t.Fatalf("plaintext backup %s remains after encrypted checkpoint: %v", name, err)
			}
		}
	}
	seedLegacyPlaintext()
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	assertHealthyLocalBackup(t, s)

	checkpointPath := filepath.Join(checkpointDir, "latest.backup")
	for path, wantMode := range map[string]os.FileMode{
		checkpointDir: 0o700, checkpointPath: 0o600,
		dbPath + ".backup-encryption.key": 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s mode = %04o, want %04o", path, got, wantMode)
		}
	}
	assertLegacyPlaintextRemoved()

	// Simulate a crash after the encrypted checkpoint and restore-test
	// timestamp were published but before legacy cleanup completed. A no-op
	// maintenance pass must retry the idempotent cleanup.
	seedLegacyPlaintext()
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain after interrupted cleanup: %v", err)
	}
	assertLegacyPlaintextRemoved()

	// The other interruption point, after checkpoint publication but before
	// its restore-test timestamp, must also clean up once the test completes.
	if err := store.SetEncryptedCheckpointTimesForTest(files, time.Now(), nil); err != nil {
		t.Fatalf("clear restore-test timestamp: %v", err)
	}
	seedLegacyPlaintext()
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain after interrupted restore test: %v", err)
	}
	assertLegacyPlaintextRemoved()

	oldCheckpoint := time.Now().Add(-store.DefaultLocalRestoreTestRefreshInterval - 24*time.Hour)
	oldRestoreTest := time.Now().Add(-store.DefaultLocalRestoreTestRefreshInterval - time.Hour)
	if err := store.SetEncryptedCheckpointTimesForTest(
		files, oldCheckpoint, &oldRestoreTest,
	); err != nil {
		t.Fatalf("age encrypted checkpoint evidence: %v", err)
	}
	touchedAt := time.Now()
	if err := os.Chtimes(checkpointPath, touchedAt, touchedAt); err != nil {
		t.Fatalf("touch old checkpoint: %v", err)
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

	// Refute deletion-before-proof: an invalid encrypted checkpoint must fail
	// before cleanup touches the last legacy fallback.
	legacyFallbackPath := filepath.Join(checkpointDir, "latest.db")
	if err := os.WriteFile(
		legacyFallbackPath, []byte("legacy credential-bearing bytes"), 0o600,
	); err != nil {
		t.Fatalf("seed legacy fallback: %v", err)
	}
	checkpointBody, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned checkpoint path
	if err != nil {
		t.Fatalf("read checkpoint for tamper: %v", err)
	}
	checkpointBody[len(checkpointBody)-1] ^= 0xff
	if err := os.WriteFile( //nolint:gosec // test-owned checkpoint path
		checkpointPath, checkpointBody, 0o600,
	); err != nil {
		t.Fatalf("tamper checkpoint: %v", err)
	}
	if err := producer.Maintain(ctx); err == nil {
		t.Fatal("Maintain accepted a tampered encrypted checkpoint")
	}
	if _, err := os.Lstat(legacyFallbackPath); err != nil {
		t.Fatalf("failed maintenance removed the legacy fallback: %v", err)
	}
}

func TestLocalBackupProducerPreservesFallbackUntilManifestRefreshSucceeds(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	firstDigest := domain.Digest("sha256:manifest-first")
	secondDigest := domain.Digest("sha256:manifest-second")
	phase := "initial"
	failingRefreshCalls := 0
	extractors := map[string]store.BackupPayloadDigestExtractor{
		"backup.policy-change": func(store.QueueEntry) ([]domain.Digest, error) {
			switch phase {
			case "initial":
				return []domain.Digest{firstDigest}, nil
			case "failing-refresh":
				failingRefreshCalls++
				if failingRefreshCalls == 1 {
					return []domain.Digest{firstDigest, secondDigest}, nil
				}
				return nil, fmt.Errorf("forced manifest refresh failure")
			case "successful-refresh":
				return []domain.Digest{firstDigest, secondDigest}, nil
			default:
				return nil, fmt.Errorf("unexpected extractor phase %q", phase)
			}
		},
	}
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(
		backupArtifactSet{firstDigest: true, secondDigest: true}, nil, extractors)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{BackupHealthSource: source})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			ctx, "manifest-policy-change", "backup.policy-change", []byte("payload"))
		return err
	}); err != nil {
		t.Fatalf("enqueue durable task: %v", err)
	}
	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("initial Maintain: %v", err)
	}

	checkpointDir := dbPath + ".checkpoints"
	checkpointPath := filepath.Join(checkpointDir, "latest.backup")
	initialCheckpoint, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("read initial checkpoint: %v", err)
	}
	legacyFallbackPath := filepath.Join(checkpointDir, "latest.db")
	if err := os.WriteFile(
		legacyFallbackPath, []byte("legacy credential-bearing bytes"), 0o600,
	); err != nil {
		t.Fatalf("seed legacy fallback: %v", err)
	}

	phase = "failing-refresh"
	if err := producer.Maintain(ctx); err == nil {
		t.Fatal("Maintain accepted a manifest change whose replacement failed")
	}
	if _, err := os.Lstat(legacyFallbackPath); err != nil {
		t.Fatalf("failed manifest refresh removed the legacy fallback: %v", err)
	}
	failedCheckpoint, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("read checkpoint after failed refresh: %v", err)
	}
	if !bytes.Equal(failedCheckpoint, initialCheckpoint) {
		t.Fatal("failed manifest refresh replaced the last encrypted checkpoint")
	}

	phase = "successful-refresh"
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("successful manifest refresh: %v", err)
	}
	if _, err := os.Lstat(legacyFallbackPath); !os.IsNotExist(err) {
		t.Fatalf("validated manifest refresh left the legacy fallback: %v", err)
	}
	refreshedCheckpoint, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("read refreshed checkpoint: %v", err)
	}
	if bytes.Equal(refreshedCheckpoint, initialCheckpoint) {
		t.Fatal("manifest policy change did not replace the encrypted checkpoint")
	}
	assertHealthyLocalBackup(t, s)
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
	if _, err := os.Stat(filepath.Join(checkpointDir, "latest.backup")); !os.IsNotExist(err) {
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
	checkpointPath := filepath.Join(checkpointDir, "latest.backup")
	oldCheckpoint := time.Now().Add(-store.DefaultLocalCheckpointRefreshInterval - time.Hour)
	oldRestore := oldCheckpoint.Add(time.Hour)
	if err := store.SetEncryptedCheckpointTimesForTest(
		files, oldCheckpoint, &oldRestore,
	); err != nil {
		t.Fatalf("age encrypted checkpoint: %v", err)
	}
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
	select {
	case err := <-maintainDone:
		t.Fatalf("producer finished while health held its generation lease: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

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
