package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// bindTestSubject supplies a run parent for item-focused tests. Tests of
// production lineage construct the complete run before calling this helper.
func bindTestSubject(ctx context.Context, tx *WriteTx, item *domain.AttentionItem) error {
	var runID domain.RunID
	if item.Subject.RunID != nil {
		runID = *item.Subject.RunID
	} else if item.Subject.Type == domain.SubjectRun {
		runID = domain.RunID(item.Subject.ID)
	}
	if runID == "" {
		return nil
	}
	run, err := tx.GetRun(ctx, runID)
	if errors.Is(err, ErrNotFound) {
		run = domain.Run{
			ID: runID, ProjectID: item.ProjectID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		}
		if err := tx.AssignTask(ctx, &run, nil); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	item.Subject.TaskID = &run.TaskID
	return nil
}

func putTestAttentionItem(ctx context.Context, tx *WriteTx, item *domain.AttentionItem) error {
	if err := bindTestSubject(ctx, tx, item); err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, *item)
}

// Raw attention fixtures bypass PutAttentionItem to target reconstruction.
// Supply the task prerequisites only on schemas that already contain them;
// historical migration fixtures retain their original row shape.
func bindRawItemTask(t *testing.T, ctx context.Context, db execer, item *domain.AttentionItem) {
	t.Helper()
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('attention_items') WHERE name = 'subject_task_id'`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present == 0 || (item.Subject.RunID == nil && item.Subject.Type != domain.SubjectRun) {
		return
	}
	var raw *sql.Tx
	owned := false
	switch db := db.(type) {
	case *sql.Tx:
		raw = db
	case *sql.DB:
		var err error
		raw, err = db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Rollback() }()
		owned = true
	default:
		t.Fatalf("unsupported raw fixture database %T", db)
	}
	writer := &WriteTx{InternalTx: InternalTx{ReadTx: ReadTx{tx: raw}}, asOfRevision: 1}
	runID := domain.RunID(item.Subject.ID)
	if item.Subject.RunID != nil {
		runID = *item.Subject.RunID
	}
	var taskID domain.TaskID
	err := raw.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id = ?`, runID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		err = bindTestSubject(ctx, writer, item)
	} else if err == nil {
		item.Subject.TaskID = &taskID
	}
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		if err := raw.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

// insertRawRunFixture preserves independently supplied columns/body/metadata,
// including deliberate corruption, while supplying the new task prerequisites.
func insertRawRunFixture(ctx context.Context, db *sql.DB, id, projectID, policy string, version, revision int64, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	writer := &WriteTx{InternalTx: InternalTx{ReadTx: ReadTx{tx: tx}}, asOfRevision: 1}
	task, err := writer.getOrCreateTask(ctx, domain.ProjectID(projectID), "orphan:"+id, nil, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &object); err != nil {
		return err
	}
	object["task_id"], err = json.Marshal(task.ID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs (id, project_id, task_id, policy_digest, entity_version, as_of_revision, body)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, projectID, task.ID, policy, version, revision, string(encoded)); err != nil {
		return err
	}
	if err := writer.recordTaskRun(ctx, domain.Run{ID: domain.RunID(id), ProjectID: domain.ProjectID(projectID), TaskID: task.ID}); err != nil {
		return err
	}
	return tx.Commit()
}

func TestTaskReconstructionRejectsCoherentRunRetarget(t *testing.T) {
	ctx := t.Context()
	db := openRaw(t)
	dump, err := os.ReadFile(filepath.Join("testdata", "pre_rename_specification_vocabulary.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(dump)); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	var run domain.Run
	var task domain.Task
	if err := st.Write(ctx, func(tx *WriteTx) error {
		var err error
		run, err = tx.GetRun(ctx, "implementation-from-submit")
		if err != nil {
			return err
		}
		task, err = tx.getOrCreateTask(ctx, run.ProjectID, "orphan:unrelated-run", nil, time.Now().UTC())
		if err != nil {
			return err
		}
		task.CampaignIDs = []domain.CampaignID{run.CampaignID}
		return tx.updateTask(ctx, task)
	}); err != nil {
		t.Fatal(err)
	}
	run.TaskID = task.ID
	body, err := encode(run)
	if err != nil {
		t.Fatal(err)
	}
	// Align all mutable task coordinates. The immutable campaign relationship
	// still binds this implementation to the specification's original task.
	if _, err := db.ExecContext(ctx, `UPDATE runs SET task_id = ?, body = ? WHERE id = ?`, task.ID, body, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_runs SET task_id = ?, ordinal = 1 WHERE run_id = ?`, task.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error { _, err := tx.GetRun(ctx, run.ID); return err }); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("coherent task retarget = %v, want parent mismatch", err)
	}
}

func TestTaskMigrationDoesNotGroupOrphanThroughForgedAttemptColumn(t *testing.T) {
	ctx := t.Context()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0070_")
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	orphan := domain.Run{ID: "run-orphan", ProjectID: "project-1", SpecDigest: "sha256:orphan", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}
	body, err := encode(orphan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES (?, ?, ?, 1, 1, ?)`, orphan.ID, orphan.ProjectID, orphan.PolicyDigest, body); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	attempt := testInitialProductionAttempt()
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutProductionAttempt(ctx, attempt) }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE production_attempts SET specification_run_id = ?`, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		run, err := tx.GetRun(ctx, orphan.ID)
		if err != nil {
			return err
		}
		var key string
		if err := tx.tx.QueryRowContext(ctx, `SELECT intake_key FROM task_intake_keys WHERE task_id = ?`, run.TaskID).Scan(&key); err != nil {
			return err
		}
		if key != "orphan:"+string(orphan.ID) {
			t.Fatalf("forged attempt supplied intake key %q", key)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskMigrationAuthenticatesStartedLabelIntake(t *testing.T) {
	for _, mutation := range []string{"", "declaration", "occurrence"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := t.Context()
			db := openRaw(t)
			migrateThrough(t, ctx, db, "0070_")
			dump, err := os.ReadFile(filepath.Join("testdata", "pre_tasks_started_label_intake.sql"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, string(dump)); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "declaration":
				_, err = db.ExecContext(ctx, `UPDATE work_unit_declarations SET project_id = 'forged-project'`)
			case "occurrence":
				_, err = db.ExecContext(ctx, `UPDATE intake_occurrences SET ordinal = ordinal + 1`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			st := &Store{db: db}
			if err := st.Write(ctx, func(tx *WriteTx) error {
				var runID domain.RunID
				var taskID domain.TaskID
				if err := tx.tx.QueryRowContext(ctx, `SELECT id, task_id FROM runs`).Scan(&runID, &taskID); err != nil {
					return err
				}
				task, err := tx.GetTask(ctx, taskID)
				if err != nil {
					return err
				}
				source := domain.SpecificationSource{
					Kind:         domain.SpecificationSourceIssueSubject,
					IssueSubject: &domain.IssueSubjectRef{Repo: "freeasinbird/freeside", RepositoryID: 84958515, IssueNumber: 7},
				}
				replay, err := tx.GetOrCreateTask(ctx, "project-label-intake", source)
				if err != nil {
					return err
				}
				if mutation != "" {
					if replay.ID == taskID || (task.Source != nil && task.Source.Kind == domain.SpecificationSourceIssueSubject) {
						t.Fatalf("forged %s retained issue intake authority: %+v", mutation, task)
					}
					return nil
				}
				if replay.ID != taskID || task.Source == nil || *task.Source.IssueSubject != *source.IssueSubject {
					t.Fatalf("started label intake lost its task: original=%+v replay=%+v", task, replay)
				}
				run, err := tx.GetRun(ctx, runID)
				if err == nil && (run.TaskID != taskID || len(run.Stages) == 0) {
					t.Fatalf("started run did not reconstruct: %+v", run)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTaskMigrationRetainsUnstartedLabelIntake(t *testing.T) {
	ctx := t.Context()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0070_")
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	raw, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Rollback() }()
	writer := &WriteTx{InternalTx: InternalTx{ReadTx: ReadTx{tx: raw, beforeTasks: true}}, asOfRevision: 1}
	occurrence, _, err := writer.AllocateNextIntakeOccurrence(ctx, intakeIntRepo, intakeIntRepoID, intakeIntIssue, intakeIntLabel, intakeIntTS)
	if err != nil {
		t.Fatal(err)
	}
	implementationID := domain.IntakeImplementationRunID(occurrence)
	specificationID := domain.SpecificationRunIDForImplementation(implementationID)
	policy, err := domain.NewResolvedPolicy(specificationID, []domain.PolicyKey{{Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: "sha256:policy-source"}}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: specificationID, ProjectID: "project-1", SpecDigest: "sha256:label-source", PolicyDigest: policy.Digest,
		CampaignID: derivedInitialCampaignID(implementationID), AttemptNumber: 1, Stages: []domain.Stage{},
	}
	if err := writer.PutProductionAttempt(ctx, domain.ProductionAttempt{
		CampaignID: run.CampaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
		SourceDigest: run.SpecDigest, PublicationDigest: "sha256:publication", SpecificationRunID: run.ID, ImplementationRunID: implementationID,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := encode(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO runs (id, project_id, policy_digest, campaign_id, attempt_number, entity_version, as_of_revision, body) VALUES (?, ?, ?, ?, 1, 1, 1, ?)`, run.ID, run.ProjectID, policy.Digest, run.CampaignID, body); err != nil {
		t.Fatal(err)
	}
	if err := writer.PutResolvedPolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	project, err := domain.NewProject(run.ProjectID, intakeIntRepo, intakeIntRepoID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.RegisterProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.MintIntakeDeclaration(ctx, intakeIntRepoID, intakeIntIssue, intakeIntLabel, occurrence.Ordinal, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := raw.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		migrated, err := tx.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		source := intakeIssueSubjectSource(occurrence)
		originalTask := migrated.TaskID
		if err := tx.AssignTask(ctx, &migrated, &source); err != nil {
			return err
		}
		migrated.Stages = []domain.Stage{{ID: domain.SpecificationStageID(run.ID), RunID: run.ID, Name: string(domain.StageNameSpecification), Attempts: []domain.Attempt{}}}
		if err := tx.PutRun(ctx, migrated); err != nil {
			return err
		}
		repeated, err := tx.GetOrCreateTask(ctx, run.ProjectID, source)
		if err != nil {
			return err
		}
		if migrated.TaskID != originalTask || repeated.ID != originalTask || repeated.Source == nil || repeated.Source.Kind != domain.SpecificationSourceIssueSubject {
			t.Fatalf("queued intake lost its task: started=%q original=%q repeat=%+v", migrated.TaskID, originalTask, repeated)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
