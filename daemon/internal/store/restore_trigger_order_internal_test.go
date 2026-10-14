package store

import (
	"context"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
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

// insertTriggerRestoreHandling classifies every insert trigger in the
// migrated schema by how restore copes with it.
var insertTriggerRestoreHandling = map[string]string{
	// The trigger reads rows in other tables, so restore copies its table
	// after every other table (restoreLateCopyTables).
	"runs_task_insert":                                     "late-copy",
	"finding_disposition_requires_round_finding":           "late-copy",
	"external_finding_disposition_requires_admitted_cycle": "late-copy",
	// Restore drops the trigger and reinstates it before commit.
	outboxPublicationInsertTrigger: "suspended",
	// The trigger only rejects a row that conflicts with one already in
	// another table; a consistent checkpoint holds no such pair, and the
	// trigger needs nothing copied first.
	"execution_exports_exclude_outcomes":       "exclusion",
	"execution_outcomes_exclude_exports":       "exclusion",
	"review_record_rejects_failure":            "exclusion",
	"review_failure_rejects_record":            "exclusion",
	"shadow_review_rejects_routed_invocation":  "exclusion",
	"routed_review_rejects_shadow_invocation":  "exclusion",
	"review_failure_rejects_shadow_invocation": "exclusion",
	"review_retry_rejects_shadow_invocation":   "exclusion",
	"routed_review_finding_rejects_shadow":     "exclusion",
	"shadow_review_finding_rejects_routed":     "exclusion",
}

var insertTriggerSQL = regexp.MustCompile(`(?is)^\s*CREATE\s+TRIGGER\s+(?:IF\s+NOT\s+EXISTS\s+)?\S+\s+(?:BEFORE\s+|AFTER\s+|INSTEAD\s+OF\s+)?INSERT\s+ON\s`)

// TestRestoreClassifiesEveryInsertTrigger keeps restore's copy order in step
// with the schema: a new insert trigger fails here until it is classified,
// and a trigger that reads other tables must have its table registered in
// restoreLateCopyTables, or restore of a store holding its rows fails.
func TestRestoreClassifiesEveryInsertTrigger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	rows, err := st.db.QueryContext(ctx,
		`SELECT name, tbl_name, sql FROM sqlite_master WHERE type = 'trigger' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	var lateTables []string
	for rows.Next() {
		var name, table, definition string
		if err := rows.Scan(&name, &table, &definition); err != nil {
			t.Fatal(err)
		}
		if !insertTriggerSQL.MatchString(definition) {
			// A header the pattern cannot read must not let an insert
			// trigger skip classification.
			header, _, _ := strings.Cut(strings.ToUpper(definition), "BEGIN")
			if strings.Contains(header, "INSERT") {
				t.Errorf("cannot read the event of trigger %s: %s", name, header)
			}
			continue
		}
		seen[name] = true
		switch insertTriggerRestoreHandling[name] {
		case "late-copy":
			if !slices.Contains(restoreLateCopyTables, table) {
				t.Errorf("trigger %s reads other tables, but its table %s is not in restoreLateCopyTables",
					name, table)
			}
			lateTables = append(lateTables, table)
		case "suspended", "exclusion":
		default:
			t.Errorf("insert trigger %s on %s is unclassified: if it requires rows in another table, "+
				"register %s in restoreLateCopyTables (restore.go); then classify it here", name, table, table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range insertTriggerRestoreHandling {
		if !seen[name] {
			t.Errorf("classified insert trigger %s is not in the schema", name)
		}
	}
	for _, table := range restoreLateCopyTables {
		if !slices.Contains(lateTables, table) {
			t.Errorf("restoreLateCopyTables lists %s, but no late-copy trigger is on it", table)
		}
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
