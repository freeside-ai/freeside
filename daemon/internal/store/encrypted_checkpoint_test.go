package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	// A janitor mint hold must survive restore, or a restored daemon could
	// mint while an earlier token may still be live (#2004).
	mintHold := installationMintHoldFixture()
	if err := recordMintHold(t, s, mintHold); err != nil {
		t.Fatalf("seed mint hold: %v", err)
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
	if got, found := readMintHold(t, s, mintHold.InstallationID); !found || !mintHoldEqual(got, mintHold) {
		t.Fatalf("restored mint hold = %+v, %v; want %+v", got, found, mintHold)
	}
}

// Restore proves every attachment blob before it overwrites a store, from both
// places an attachment is named: the task that carried it and the stage-input
// snapshot an admission records.
func TestRestoreChecksEveryAttachmentBlob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	f, stageInputDigests := stageInputAdmissionFixture(t)
	snapshotAttachments := f.admission.StageInputs.Attachments
	// Distinct from every stage-input digest, so a missing task attachment
	// is not also a missing snapshot role.
	taskAttachments := sampleTaskAttachments()
	taskAttachments[0].Digest = domain.Digest("sha256:" + strings.Repeat("a", 64))
	taskAttachments[1].Digest = domain.Digest("sha256:" + strings.Repeat("b", 64))
	artifacts := backupArtifactSet{}
	for _, digest := range stageInputDigests {
		artifacts[digest] = true
	}
	for _, attachment := range taskAttachments {
		artifacts[attachment.Digest] = true
	}

	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(artifacts, approvedFixtureRecipes(), nil)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{
		AdmissionFloors: attendedFloors(), ApprovedRecipes: approvedFixtureRecipes(),
		BackupHealthSource: source,
	})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	if err := putTaskAttachments(ctx, s, taskID, taskAttachments); err != nil {
		t.Fatalf("PutTaskAttachments: %v", err)
	}
	recordStageInputAdmission(t, s, f)
	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	if err := producer.Maintain(ctx); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	// A row written after the checkpoint shows whether a restore replaced
	// the live store.
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, newItem(t, "post-checkpoint-row", nil, 1))
	}); err != nil {
		t.Fatalf("advance live state: %v", err)
	}
	liveRowSurvives := func() bool {
		t.Helper()
		err := s.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetAttentionItem(ctx, "post-checkpoint-row")
			return err
		})
		return err == nil
	}

	for _, missing := range []domain.Digest{
		taskAttachments[0].Digest, taskAttachments[1].Digest, snapshotAttachments[0].Digest,
	} {
		artifacts[missing] = false
		if _, _, err := files.RestoreCheckpoint(ctx, s); !errors.Is(
			err, domain.ErrArtifactClosureIncomplete,
		) {
			t.Fatalf("RestoreCheckpoint missing %s = %v, want %v",
				missing, err, domain.ErrArtifactClosureIncomplete)
		}
		if !liveRowSurvives() {
			t.Fatalf("a refused restore missing %s replaced the live store", missing)
		}
		artifacts[missing] = true
	}

	if _, _, err := files.RestoreCheckpoint(ctx, s); err != nil {
		t.Fatalf("RestoreCheckpoint with every blob present: %v", err)
	}
	if liveRowSurvives() {
		t.Fatal("post-checkpoint row survived restore")
	}
	gotTask, err := readTaskAttachments(ctx, s, taskID)
	if err != nil {
		t.Fatalf("restored task attachments: %v", err)
	}
	if !slices.Equal(gotTask, taskAttachments) {
		t.Fatalf("restored task attachments = %+v, want %+v", gotTask, taskAttachments)
	}
	var restored domain.ExecutionAdmission
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		restored, err = tx.GetExecutionAdmission(ctx, f.admission.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("restored admission: %v", err)
	}
	if restored.StageInputs == nil ||
		!slices.Equal(restored.StageInputs.Attachments, snapshotAttachments) {
		t.Fatalf("restored snapshot attachments = %+v, want %+v",
			restored.StageInputs, snapshotAttachments)
	}
}

// The producer seals what the closure scan reads, so a task attachment row
// that fails validation stops the pass with a hard error, not a closure gap:
// Run keeps going past a gap, but this row is state the store owns, not a
// payload another binary wrote, and no checkpoint may be sealed over it.
func TestProducerFailsLoudlyOnAnInvalidTaskAttachment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	artifacts := backupArtifactSet{}
	for _, attachment := range sampleTaskAttachments() {
		artifacts[attachment.Digest] = true
	}
	files, err := store.NewDefaultLocalBackupFiles(dbPath)
	if err != nil {
		t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
	}
	source, err := files.NewCheckpointHealthSource(artifacts, nil, nil)
	if err != nil {
		t.Fatalf("NewCheckpointHealthSource: %v", err)
	}
	s := openStoreAt(t, dbPath, store.Options{BackupHealthSource: source})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	if err := putTaskAttachments(ctx, s, taskID, sampleTaskAttachments()); err != nil {
		t.Fatalf("PutTaskAttachments: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := db.ExecContext(ctx,
		`UPDATE task_attachments SET name = '../escape' WHERE position = 0`)
	if err := errors.Join(execErr, db.Close()); err != nil {
		t.Fatalf("tamper task attachment: %v", err)
	}

	producer, err := files.NewProducer(s)
	if err != nil {
		t.Fatalf("NewProducer: %v", err)
	}
	err = producer.Maintain(ctx)
	if !errors.Is(err, store.ErrRowInconsistent) {
		t.Fatalf("Maintain = %v, want %v", err, store.ErrRowInconsistent)
	}
	if errors.Is(err, store.ErrBackupClosureIncomplete) {
		t.Fatalf("Maintain = %v, want a hard failure, not a closure gap", err)
	}
	if _, err := os.Stat(dbPath + ".checkpoints/latest.backup"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint after a failed pass: stat err = %v, want not exist", err)
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
