package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestEncryptedCheckpointRoundTripAndPlaintextProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(backupArtifactSet{}, nil, nil)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{BackupHealthSource: source})

	credentialValue := "sha256:" + strings.Repeat("ab", 32)
	fixture := newFixtures(t)
	fixture.credential.Credential = credentialValue
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDevice(ctx, fixture.device); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, newItem(t, "checkpoint-row", nil, 1))
	}); err != nil {
		t.Fatalf("seed checkpoint rows: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordDeviceCredential(ctx, fixture.credential)
	}); err != nil {
		t.Fatalf("seed credential verifier: %v", err)
	}
	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState before checkpoint: %v", err)
	}
	beforeSchema, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion before checkpoint: %v", err)
	}

	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	artifactPath := dbPath + ".checkpoints/latest.backup"
	body, err := os.ReadFile(artifactPath) //nolint:gosec // test-owned checkpoint
	if err != nil {
		t.Fatalf("read encrypted checkpoint: %v", err)
	}
	if bytes.Contains(body, []byte(credentialValue)) {
		t.Fatal("encrypted checkpoint contains the credential-bearing value in plaintext")
	}
	if bytes.Contains(body, []byte("SQLite format 3")) {
		t.Fatal("encrypted checkpoint contains a plaintext SQLite header")
	}
	health, err := s.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if health != healthyBackupHealth() {
		t.Fatalf("BackupHealth = %+v, want %+v", health, healthyBackupHealth())
	}

	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, newItem(t, "post-checkpoint-row", nil, 1))
	}); err != nil {
		t.Fatalf("advance live state: %v", err)
	}
	checkpoint, restored, err := files.RestoreCheckpoint(ctx, s)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	if checkpoint.SyncEpoch != before.SyncEpoch ||
		checkpoint.ServerRevision != before.Revision {
		t.Fatalf("checkpoint state = %q/%d, want %q/%d",
			checkpoint.SyncEpoch, checkpoint.ServerRevision,
			before.SyncEpoch, before.Revision)
	}
	if restored.Revision != before.Revision {
		t.Fatalf("restored revision = %d, want %d", restored.Revision, before.Revision)
	}
	if restored.SyncEpoch == "" || restored.SyncEpoch == before.SyncEpoch {
		t.Fatalf("restored epoch = %q, want fresh epoch distinct from %q",
			restored.SyncEpoch, before.SyncEpoch)
	}
	afterSchema, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion after restore: %v", err)
	}
	if afterSchema != beforeSchema {
		t.Fatalf("restored schema version = %d, want %d", afterSchema, beforeSchema)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(ctx, "checkpoint-row"); err != nil {
			return err
		}
		if _, err := tx.GetAttentionItem(ctx, "post-checkpoint-row"); err == nil {
			return errors.New("post-checkpoint row survived restore")
		}
		credential, err := tx.GetDeviceCredential(ctx, fixture.credential.DeviceID)
		if err != nil {
			return err
		}
		if credential.Credential != credentialValue {
			return errors.New("credential-bearing row changed during restore")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify restored rows: %v", err)
	}
}

// Restore is the one caller that must not tolerate a durable row it cannot
// read: the daemon keeps running on an incomplete closure (#430), but an older
// binary restoring a newer daemon's checkpoint would admit rows whose blobs it
// never proved present.
func TestRestoreFailsClosedOnACheckpointThisBinaryCannotScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	payloadDigest := domain.Digest("sha256:durable-task-payload")
	artifacts := backupArtifactSet{payloadDigest: true}
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(artifacts, nil,
		map[string]store.BackupPayloadDigestExtractor{
			"backup.marker": func(store.QueueEntry) ([]domain.Digest, error) {
				return []domain.Digest{payloadDigest}, nil
			},
		})
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{BackupHealthSource: source})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "marker-1", "backup.marker", []byte("payload"))
		return err
	}); err != nil {
		t.Fatalf("enqueue durable task: %v", err)
	}
	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}

	// The same checkpoint read by a binary that does not know the kind.
	downgraded, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles for the downgrade: %v", err)
	}
	downgradedSource, err := downgraded.NewCheckpointHealthSource(artifacts, nil, nil)
	if err != nil {
		t.Fatalf("downgraded health source: %v", err)
	}
	if _, _, err := downgraded.RestoreCheckpoint(ctx, s); !errors.Is(
		err, store.ErrBackupClosureIncomplete,
	) {
		t.Fatalf("RestoreCheckpoint = %v, want ErrBackupClosureIncomplete", err)
	}
	health, err := downgradedSource.BackupHealth(ctx, backupHealthContext(t, s))
	if err != nil {
		t.Fatalf("downgraded BackupHealth: %v", err)
	}
	unusable := domain.BackupHealth{
		Encryption:         domain.BackupHealthUnhealthy,
		CheckpointCurrency: domain.BackupHealthUnhealthy,
		ArtifactClosure:    domain.BackupHealthUnhealthy,
		RestoreTestAge:     domain.BackupHealthUnhealthy,
	}
	if health != unusable {
		t.Fatalf("downgraded health = %+v, want %+v", health, unusable)
	}
}

func TestEncryptedCheckpointFailsClosedOnWrongKeyTamperAndDigestMismatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T, string, *store.LocalBackupFiles) *store.LocalBackupFiles
		wantErr error
	}{
		{
			name: "wrong key",
			corrupt: func(
				t *testing.T, checkpointPath string, _ *store.LocalBackupFiles,
			) *store.LocalBackupFiles {
				t.Helper()
				wrong, err := store.NewEncryptedLocalBackupFiles(
					checkpointPath, bytes.Repeat([]byte{0x5a}, 32))
				if err != nil {
					t.Fatalf("NewEncryptedLocalBackupFiles: %v", err)
				}
				if _, err := wrong.NewCheckpointHealthSource(backupArtifactSet{}, nil, nil); err != nil {
					t.Fatalf("wrong-key health source: %v", err)
				}
				return wrong
			},
			wantErr: domain.ErrCheckpointAuthentication,
		},
		{
			name: "ciphertext tamper",
			corrupt: func(
				t *testing.T, checkpointPath string, files *store.LocalBackupFiles,
			) *store.LocalBackupFiles {
				t.Helper()
				body, err := os.ReadFile(checkpointPath) //nolint:gosec // test-owned checkpoint
				if err != nil {
					t.Fatalf("read checkpoint: %v", err)
				}
				body[len(body)/2] ^= 0x01
				if err := os.WriteFile(checkpointPath, body, 0o600); err != nil { //nolint:gosec // test-owned path
					t.Fatalf("tamper checkpoint: %v", err)
				}
				return files
			},
			wantErr: domain.ErrCheckpointAuthentication,
		},
		{
			name: "checkpoint symlink",
			corrupt: func(
				t *testing.T, checkpointPath string, files *store.LocalBackupFiles,
			) *store.LocalBackupFiles {
				t.Helper()
				target := checkpointPath + ".target"
				if err := os.Rename(checkpointPath, target); err != nil {
					t.Fatalf("move checkpoint behind symlink: %v", err)
				}
				if err := os.Symlink(target, checkpointPath); err != nil {
					t.Fatalf("symlink checkpoint: %v", err)
				}
				return files
			},
			wantErr: domain.ErrCheckpointAuthentication,
		},
		{
			name: "digest mismatch",
			corrupt: func(
				t *testing.T, _ string, files *store.LocalBackupFiles,
			) *store.LocalBackupFiles {
				t.Helper()
				if err := store.CorruptEncryptedCheckpointDigestForTest(files); err != nil {
					t.Fatalf("corrupt checkpoint digest: %v", err)
				}
				return files
			},
			wantErr: domain.ErrCheckpointDigestMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			files, err := store.NewDefaultLocalBackupFiles(dbPath)
			if err != nil {
				t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
			}
			source, err := files.NewCheckpointHealthSource(backupArtifactSet{}, nil, nil)
			if err != nil {
				t.Fatalf("NewCheckpointHealthSource: %v", err)
			}
			s := openStoreAt(t, dbPath, store.Options{BackupHealthSource: source})
			producer, err := files.NewProducer(s)
			if err != nil {
				t.Fatalf("NewProducer: %v", err)
			}
			if err := producer.Maintain(ctx); err != nil {
				t.Fatalf("Maintain: %v", err)
			}

			checkpointPath := dbPath + ".checkpoints/latest.backup"
			restoreFiles := tc.corrupt(t, checkpointPath, files)
			corruptSource, err := restoreFiles.NewCheckpointHealthSource(
				backupArtifactSet{}, nil, nil)
			if err != nil {
				t.Fatalf("corrupt checkpoint health source: %v", err)
			}
			health, err := corruptSource.BackupHealth(ctx, backupHealthContext(t, s))
			if err != nil {
				t.Fatalf("corrupt checkpoint health: %v", err)
			}
			if health.Encryption != domain.BackupHealthUnhealthy {
				t.Fatalf("corrupt checkpoint encryption = %q, want unhealthy", health.Encryption)
			}
			if _, _, err := restoreFiles.RestoreCheckpoint(ctx, s); !errors.Is(err, tc.wantErr) {
				t.Fatalf("RestoreCheckpoint = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
