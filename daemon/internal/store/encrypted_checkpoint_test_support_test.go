package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestBackupEncryptionKeyFailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("missing beside encrypted checkpoint", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		if _, err := NewDefaultLocalBackupFiles(dbPath); err != nil {
			t.Fatalf("create backup key: %v", err)
		}
		if err := os.Remove(dbPath + backupEncryptionKeySuffix); err != nil {
			t.Fatalf("remove backup key: %v", err)
		}
		checkpointDir := dbPath + ".checkpoints"
		if err := os.Mkdir(checkpointDir, 0o700); err != nil {
			t.Fatalf("create checkpoint directory: %v", err)
		}
		if err := os.WriteFile(
			filepath.Join(checkpointDir, encryptedCheckpointFilename),
			[]byte("encrypted artifact"), 0o600,
		); err != nil {
			t.Fatalf("write checkpoint marker: %v", err)
		}
		if _, err := NewDefaultLocalBackupFiles(dbPath); !errors.Is(err, errBackupKeyMissing) {
			t.Fatalf("missing key error = %v, want %v", err, errBackupKeyMissing)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "freeside.db")
		target := filepath.Join(root, "target.key")
		if err := os.WriteFile(target, make([]byte, backupEncryptionKeySize), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Symlink(target, dbPath+backupEncryptionKeySuffix); err != nil {
			t.Fatalf("symlink backup key: %v", err)
		}
		if _, err := NewDefaultLocalBackupFiles(dbPath); !errors.Is(err, errBackupKeyPermissions) {
			t.Fatalf("symlink key error = %v, want %v", err, errBackupKeyPermissions)
		}
	})

	t.Run("hard link", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "freeside.db")
		target := filepath.Join(root, "target.key")
		if err := os.WriteFile(target, make([]byte, backupEncryptionKeySize), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Link(target, dbPath+backupEncryptionKeySuffix); err != nil {
			t.Fatalf("hard-link backup key: %v", err)
		}
		if _, err := NewDefaultLocalBackupFiles(dbPath); !errors.Is(err, errBackupKeyPermissions) {
			t.Fatalf("hard-linked key error = %v, want %v", err, errBackupKeyPermissions)
		}
	})
}

func TestConcurrentBackupEncryptionKeyCreationConverges(t *testing.T) {
	t.Parallel()
	const contenders = 32
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	start := make(chan struct{})
	keys := make([][]byte, contenders)
	errs := make([]error, contenders)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(contenders)
	done.Add(contenders)
	for i := range contenders {
		go func(index int) {
			defer done.Done()
			ready.Done()
			<-start
			files, err := NewDefaultLocalBackupFiles(dbPath)
			if err == nil {
				keys[index] = files.encryptionKey
			}
			errs[index] = err
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("contender %d: NewDefaultLocalBackupFiles: %v", i, err)
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("contender %d retained a different encryption key", i)
		}
	}
	reopened, err := NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("reopen winning backup key: %v", err)
	}
	if !bytes.Equal(reopened.encryptionKey, keys[0]) {
		t.Fatal("durable backup key differs from the converged in-memory key")
	}
}

func TestBackupEncryptionKeyPublicationDoesNotReplaceWinner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "freeside.db"+backupEncryptionKeySuffix)
	winner := bytes.Repeat([]byte{0x42}, backupEncryptionKeySize)
	if err := os.WriteFile(path, winner, 0o600); err != nil {
		t.Fatalf("write winning backup key: %v", err)
	}

	got, err := createBackupEncryptionKey(path)
	if err != nil {
		t.Fatalf("create after winner publication: %v", err)
	}
	if !bytes.Equal(got, winner) {
		t.Fatal("loser retained its candidate instead of loading the winning key")
	}
	durable, err := os.ReadFile(path) //nolint:gosec // test-owned key path
	if err != nil {
		t.Fatalf("read winning backup key: %v", err)
	}
	if !bytes.Equal(durable, winner) {
		t.Fatal("loser replaced the already published key")
	}
	temporaries, err := filepath.Glob(
		filepath.Join(root, "."+filepath.Base(path)+"-*.tmp"),
	)
	if err != nil {
		t.Fatalf("glob losing key temporary: %v", err)
	}
	if len(temporaries) != 0 {
		t.Fatalf("losing key temporaries remain: %v", temporaries)
	}
}

// MutateEncryptedCheckpointForTest simulates a key-holding adversary: it
// changes the decrypted SQLite body and rebinds only the snapshot digest. The
// production reader must still reject any semantic or artifact-manifest
// inconsistency exposed by the mutation.
func MutateEncryptedCheckpointForTest(
	ctx context.Context, files *LocalBackupFiles, statement string, args ...any,
) error {
	files.mu.Lock()
	defer files.mu.Unlock()

	plaintext, checkpoint, err := openEncryptedCheckpoint(
		files.checkpointPath, files.encryptionKey)
	if err != nil {
		return err
	}
	db, conn, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		return err
	}
	defer closeDeserializedBackupDatabase(db, conn)
	if _, err := conn.ExecContext(ctx, statement, args...); err != nil {
		return err
	}
	plaintext, err = serializeBackupConnection(conn)
	if err != nil {
		return err
	}
	checkpoint.SQLiteSnapshotDigest = digestBytes(plaintext)
	body, err := sealEncryptedCheckpoint(plaintext, checkpoint, files.encryptionKey)
	if err != nil {
		return err
	}
	return installEncryptedCheckpoint(files.dir, files.checkpointPath, body)
}

// SetEncryptedCheckpointTimesForTest changes authenticated lifecycle times
// without trusting filesystem mtimes.
func SetEncryptedCheckpointTimesForTest(
	files *LocalBackupFiles, completedAt time.Time, restoreTestedAt *time.Time,
) error {
	files.mu.Lock()
	defer files.mu.Unlock()

	plaintext, checkpoint, err := openEncryptedCheckpoint(
		files.checkpointPath, files.encryptionKey)
	if err != nil {
		return err
	}
	checkpoint.CreatedAt = completedAt.UTC()
	checkpoint.CompletedAt = completedAt.UTC()
	if restoreTestedAt == nil {
		checkpoint.RestoreTestedAt = nil
	} else {
		value := restoreTestedAt.UTC()
		checkpoint.RestoreTestedAt = &value
	}
	body, err := sealEncryptedCheckpoint(plaintext, checkpoint, files.encryptionKey)
	if err != nil {
		return err
	}
	return installEncryptedCheckpoint(files.dir, files.checkpointPath, body)
}

// CorruptEncryptedCheckpointDigestForTest produces a correctly authenticated
// envelope whose advertised plaintext digest is false.
func CorruptEncryptedCheckpointDigestForTest(files *LocalBackupFiles) error {
	files.mu.Lock()
	defer files.mu.Unlock()

	plaintext, checkpoint, err := openEncryptedCheckpoint(
		files.checkpointPath, files.encryptionKey)
	if err != nil {
		return err
	}
	checkpoint.SQLiteSnapshotDigest = domain.Digest(
		"sha256:0000000000000000000000000000000000000000000000000000000000000000")
	body, err := sealEncryptedCheckpoint(plaintext, checkpoint, files.encryptionKey)
	if err != nil {
		return err
	}
	return installEncryptedCheckpoint(files.dir, files.checkpointPath, body)
}
