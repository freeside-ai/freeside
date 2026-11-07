package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// These mirror backupEncryptionKeySuffix and encryptedCheckpointFilename in
// internal/store/encrypted_checkpoint.go, and the ".checkpoints" directory
// in its defaultLocalBackupPaths. A rename there fails the template build,
// which reads the files back by these names.
const (
	templateBackupKeySuffix     = ".backup-encryption.key"
	templateCheckpointDirSuffix = ".checkpoints"
	templateCheckpointFilename  = "latest.backup"
)

// checkpointedTemplate holds the files a daemon's first start leaves for
// backup: a migrated database with its sync epoch seeded, the backup
// encryption key, and an encrypted checkpoint of that database with
// restore-test metadata.
type checkpointedTemplate struct {
	database, backupKey, checkpoint []byte
}

// buildCheckpointedTemplate is built once per test process. A full run()
// would also write rows after its first Maintain that every templated test
// would inherit, so this repeats only the backup wiring run() does before
// that Maintain, with the default (nil) approved recipes. On an empty
// database the artifact manifest is empty, so a test with its own recipes
// still finds the checkpoint current.
var buildCheckpointedTemplate = sync.OnceValues(func() (checkpointedTemplate, error) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "freesided-template-*")
	if err != nil {
		return checkpointedTemplate{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dbPath := filepath.Join(dir, "freeside.db")

	database, err := store.TemplateDatabase(ctx)
	if err != nil {
		return checkpointedTemplate{}, fmt.Errorf("migrate template: %w", err)
	}
	if err := os.WriteFile(dbPath, database, 0o600); err != nil {
		return checkpointedTemplate{}, err
	}
	if _, err := loadOrCreateTopicKey(dbPath, false); err != nil {
		return checkpointedTemplate{}, fmt.Errorf("create topic key: %w", err)
	}
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		return checkpointedTemplate{}, err
	}
	blobs, err := signet.NewBlobStore(dbPath + ".blobs")
	if err != nil {
		return checkpointedTemplate{}, err
	}
	health, err := files.NewCheckpointHealthSource(blobs, nil, engine.BackupPayloadExtractors())
	if err != nil {
		return checkpointedTemplate{}, err
	}
	// The open seeds the sync epoch the checkpoint records.
	st, _, err := openStoreWithTopicKey(ctx, dbPath, store.Options{BackupHealthSource: health})
	if err != nil {
		return checkpointedTemplate{}, err
	}
	producer, err := files.NewProducer(st)
	if err != nil {
		_ = st.Close()
		return checkpointedTemplate{}, err
	}
	maintainErr := producer.Maintain(ctx)
	// Closing the last connection folds the WAL into the database file.
	if err := errors.Join(maintainErr, st.Close()); err != nil {
		return checkpointedTemplate{}, err
	}

	var tmpl checkpointedTemplate
	for _, file := range []struct {
		path string
		into *[]byte
	}{
		{dbPath, &tmpl.database},
		{dbPath + templateBackupKeySuffix, &tmpl.backupKey},
		{filepath.Join(dbPath+templateCheckpointDirSuffix, templateCheckpointFilename), &tmpl.checkpoint},
	} {
		if *file.into, err = os.ReadFile(file.path); err != nil {
			return checkpointedTemplate{}, err
		}
	}
	return tmpl, nil
})

// seedTemplateStore lays down the checkpointed template and a fresh topic
// key at dbPath when no store is there yet, and leaves an existing store
// alone so a restart keeps its rows. The command under test then opens an
// existing store with its key, finds every migration applied and the epoch
// seeded, and its first Maintain keeps the template's checkpoint, so it
// skips producing and restore-testing one. That checkpoint holds no rows: a
// test that seeds rows before its first start and needs the checkpoint to
// cover them uses seedUncheckpointedTemplateStore. Every templated root
// shares one sync epoch. Tests of first-run behaviour (a missing store or
// key, or a refusal that must leave no store) must not use it.
func seedTemplateStore(t testing.TB, dbPath string) {
	t.Helper()
	if !seedTopicKey(t, dbPath) {
		return
	}
	tmpl, err := buildCheckpointedTemplate()
	if err != nil {
		t.Fatalf("build checkpointed template: %v", err)
	}
	checkpointDir := dbPath + templateCheckpointDirSuffix
	if err := os.Mkdir(checkpointDir, 0o700); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	// The key is written, never linked: the store refuses a key whose mode
	// isn't 0600 or whose link count isn't 1, and ignores a checkpoint that
	// group or others can read.
	for _, file := range []struct {
		path string
		data []byte
	}{
		{dbPath, tmpl.database},
		{dbPath + templateBackupKeySuffix, tmpl.backupKey},
		{filepath.Join(checkpointDir, templateCheckpointFilename), tmpl.checkpoint},
	} {
		writeNewFile(t, file.path, file.data)
	}
}

// seedUncheckpointedTemplateStore is seedTemplateStore without the backup
// key and checkpoint: the first open seeds a fresh epoch, and the first
// start produces and restore-tests a checkpoint over whatever rows the test
// seeded before it. Use it when what the test asserts depends on that
// checkpoint covering those rows.
func seedUncheckpointedTemplateStore(t testing.TB, dbPath string) {
	t.Helper()
	if !seedTopicKey(t, dbPath) {
		return
	}
	storetest.Copy(t, dbPath)
}

// seedTopicKey creates the topic key for a store about to be laid down at
// dbPath, and reports false when a store is already there.
func seedTopicKey(t testing.TB, dbPath string) bool {
	t.Helper()
	if _, err := os.Stat(dbPath); err == nil {
		return false
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat store path: %v", err)
	}
	if _, err := loadOrCreateTopicKey(dbPath, false); err != nil {
		t.Fatalf("create topic key: %v", err)
	}
	return true
}

func writeNewFile(t testing.TB, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestTemplatedStartKeepsCheckpoint proves a templated start skips checkpoint
// production and the restore test: either rewrites latest.backup, so equal
// bytes after a start and stop hold only when Maintain found the template's
// checkpoint current. A start that reseeds the epoch, a database file left
// without its WAL folded in, or anything else that makes the checkpoint
// stale fails it here instead of silently slowing every daemon-starting test.
func TestTemplatedStartKeepsCheckpoint(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h := startDaemon(t, config{Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	tmpl, err := buildCheckpointedTemplate()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dbPath+templateCheckpointDirSuffix, templateCheckpointFilename)) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(tmpl.checkpoint) {
		t.Fatal("the start rewrote the template's checkpoint; Maintain produced or restore-tested instead of finding it current")
	}
}
