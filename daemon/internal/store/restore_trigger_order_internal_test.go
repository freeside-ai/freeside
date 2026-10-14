package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// restoreTargets names the stores a plaintext Restore of src's checkpoint
// can land in: src itself, whose live rows match the checkpoint, and an
// empty store, which holds none of the rows an insert trigger reads.
var restoreTargets = map[string]func(t *testing.T, src *Store) *Store{
	"over the live store": func(_ *testing.T, src *Store) *Store { return src },
	"into an empty store": func(t *testing.T, _ *Store) *Store {
		return openTemplateStoreAt(t, filepath.Join(t.TempDir(), "restored.db"), Options{})
	},
}

// TestRestoreKeepsFindingDispositions: plaintext Restore copies a
// disposition after the review round its insert trigger reads, so the row
// survives, and the trigger still guards later inserts.
func TestRestoreKeepsFindingDispositions(t *testing.T) {
	t.Parallel()
	for name, target := range restoreTargets {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			src, want, finding := seedFindingDisposition(t)
			st := target(t, src)
			restoreCheckpoint(t, src, st)
			restoreCheckpoint(t, st, st)

			var got domain.ReviewDispositionRecord
			if err := st.Read(ctx, func(tx *ReadTx) error {
				var err error
				got, err = tx.GetFindingDisposition(ctx, want.FindingID, want.Round)
				return err
			}); err != nil {
				t.Fatalf("read restored disposition: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("restored disposition = %#v, want %#v", got, want)
			}

			foreign := finding
			foreign.ID = "finding-unlisted"
			if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutFinding(ctx, foreign) }); err != nil {
				t.Fatal(err)
			}
			outside := want
			outside.FindingID = foreign.ID
			body, err := encode(outside)
			if err != nil {
				t.Fatal(err)
			}
			_, err = st.db.ExecContext(ctx, putFindingDispositionSQL,
				outside.FindingID, outside.RunID, outside.Round,
				outside.Disposition, outside.Reason, outside.RemediationInvocationID,
				formatTime(outside.CreatedAt), reviewBodyDigest(body), body)
			if err == nil || !strings.Contains(err.Error(), "finding does not belong to review round") {
				t.Fatalf("direct insert outside the round after restore = %v, want the trigger's refusal", err)
			}
		})
	}
}

// TestRestoreKeepsExternalFindingDispositions is the same check for an
// external disposition, whose trigger reads the cycle's review record and
// its outbox authority.
func TestRestoreKeepsExternalFindingDispositions(t *testing.T) {
	t.Parallel()
	for name, target := range restoreTargets {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := seedExternalCycle(t)
			want := f.decided(t, f.second, domain.ReviewDispositionDeferred)
			if err := f.put(want); err != nil {
				t.Fatalf("put: %v", err)
			}
			restored := f
			restored.st = target(t, f.st)
			restoreCheckpoint(t, f.st, restored.st)
			restoreCheckpoint(t, restored.st, restored.st)

			got, err := restored.get(want)
			if err != nil {
				t.Fatalf("read restored disposition: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("restored disposition = %#v, want %#v", got, want)
			}

			outside := want
			outside.Round = f.otherBase.Round
			err = restored.insertRaw(t, outside)
			if err == nil || !strings.Contains(err.Error(),
				"external finding does not belong to an external review cycle round") {
				t.Fatalf("direct insert outside the cycle after restore = %v, want the trigger's refusal", err)
			}
		})
	}
}

// TestLocalBackupProducerRestoresFindingDispositions: the producer's restore
// test, which copies an encrypted checkpoint into an empty store, accepts a
// store holding either kind of disposition, so the checkpoint installs with
// a recorded restore test.
func TestLocalBackupProducerRestoresFindingDispositions(t *testing.T) {
	t.Parallel()
	for name, seed := range map[string]func(*testing.T) *Store{
		"finding disposition": func(t *testing.T) *Store {
			st, _, _ := seedFindingDisposition(t)
			return st
		},
		"external finding disposition": func(t *testing.T) *Store {
			f := seedExternalCycle(t)
			if err := f.put(f.decided(t, f.second, domain.ReviewDispositionDeferred)); err != nil {
				t.Fatalf("put: %v", err)
			}
			return f.st
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := seed(t)
			files, err := NewDefaultLocalBackupFiles(filepath.Join(t.TempDir(), "freeside.db"))
			if err != nil {
				t.Fatalf("NewDefaultLocalBackupFiles: %v", err)
			}
			// The external fixture's outbox rows reference no blob this test
			// needs; the extractors only let the producer compute a complete
			// closure so it reaches its restore test.
			noBlobs := func(QueueEntry) ([]domain.Digest, error) { return nil, nil }
			if _, err := files.NewCheckpointHealthSource(schemaCheckpointArtifacts{}, nil,
				map[string]BackupPayloadDigestExtractor{
					"publish.publication":             noBlobs,
					"publication_successor_authority": noBlobs,
				}); err != nil {
				t.Fatalf("NewCheckpointHealthSource: %v", err)
			}
			producer, err := files.NewProducer(st)
			if err != nil {
				t.Fatalf("NewProducer: %v", err)
			}
			if err := producer.Maintain(ctx); err != nil {
				t.Fatalf("Maintain: %v", err)
			}
			current := schemaTestBackupHealthContext(t, ctx, st)
			snapshot, _, found, err := inspectEncryptedCheckpoint(
				ctx, current.SchemaVersion, files, true, files.approvedRecipes, files.payloadExtractors)
			if err != nil || !found {
				t.Fatalf("inspect sealed checkpoint: found=%v, err=%v", found, err)
			}
			if snapshot.restoredAt.IsZero() {
				t.Fatal("sealed checkpoint has no successful restore test")
			}
		})
	}
}

// restoreCheckpoint checkpoints src and restores that checkpoint into dst
// with the plaintext Restore path.
func restoreCheckpoint(t *testing.T, src, dst *Store) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := src.Checkpoint(ctx, path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := dst.Restore(ctx, path); err != nil {
		t.Fatalf("restore: %v", err)
	}
}
