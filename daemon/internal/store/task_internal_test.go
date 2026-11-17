package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestTaskReconstructionRegatesLifecycleFacts pins the fact-loader trust
// boundary (issue #1318 D1/G3): valid facts load, but a fact whose run is not
// the task's, whose campaign does not match its run's, or whose completion
// binding resolves to another run's unit, fails closed at reconstruction rather
// than releasing a WIP slot on a corrupt log.
func TestTaskReconstructionRegatesLifecycleFacts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// A head-schema copy, so task_lifecycle_facts exists and facts can be
	// seeded directly.
	db := openRawTemplate(t)
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	seedTask := func(id domain.TaskID) {
		task := domain.Task{
			ID: id, ProjectID: "project",
			Name:      domain.DisplayName{Text: string(id), Source: domain.DisplayNameSourceIdentifier},
			CreatedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC), CampaignIDs: []domain.CampaignID{"c1"},
		}
		body, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO tasks (id, project_id, entity_version, as_of_revision, body) VALUES (?, 'project', 1, 1, ?)`, id, string(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO task_intake_keys (project_id, intake_key, task_id) VALUES ('project', ?, ?)`, "orphan:"+string(id), id); err != nil {
			t.Fatal(err)
		}
	}
	addRun := func(taskID domain.TaskID, runID domain.RunID) {
		if _, err := db.ExecContext(ctx, `INSERT INTO runs (id, project_id, task_id, policy_digest, campaign_id, entity_version, as_of_revision, body)
			VALUES (?, 'project', ?, 'sha256:p', 'c1', 1, 1, '{}')`, runID, taskID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO task_runs (task_id, ordinal, run_id)
			SELECT ?, COALESCE(MAX(ordinal), 0) + 1, ? FROM task_runs WHERE task_id = ?`, taskID, runID, taskID); err != nil {
			t.Fatal(err)
		}
	}
	addCompletion := func(runID domain.RunID) {
		if _, err := db.ExecContext(ctx, `INSERT INTO work_unit_declarations (unit_id, run_id, project_id, body, declared_at) VALUES (?, ?, 'project', '{}', ?)`, "workunit-"+string(runID), runID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO work_unit_completions (unit_id, body, recorded_at) VALUES (?, '{}', ?)`, "workunit-"+string(runID), now); err != nil {
			t.Fatal(err)
		}
	}
	addFact := func(taskID domain.TaskID, ordinal int, kind domain.TaskLifecycleFactKind, runID domain.RunID, binding string) {
		var bind any
		if binding != "" {
			bind = binding
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO task_lifecycle_facts (task_id, ordinal, kind, run_id, campaign_id, binding_unit_id, source_id, recorded_at)
			VALUES (?, ?, ?, ?, 'c1', ?, ?, ?)`, taskID, ordinal, kind, runID, bind, string(kind), now); err != nil {
			t.Fatal(err)
		}
	}
	st := &Store{db: db}
	read := func(id domain.TaskID) error {
		return st.Read(ctx, func(rx *ReadTx) error {
			_, err := rx.GetTask(ctx, id)
			return err
		})
	}

	valid := domain.TaskID("task-valid")
	seedTask(valid)
	addRun(valid, "run-valid")
	addCompletion("run-valid")
	addFact(valid, 1, domain.TaskLifecycleStarted, "run-valid", "")
	addFact(valid, 2, domain.TaskLifecycleCompleted, "run-valid", "workunit-run-valid")
	if err := read(valid); err != nil {
		t.Fatalf("valid facts must load: %v", err)
	}

	foreign := domain.TaskID("task-foreign")
	seedTask(foreign)
	addRun(foreign, "run-foreign-owned")
	addFact(foreign, 1, domain.TaskLifecycleStarted, "run-foreign-owned", "")
	addFact(foreign, 2, domain.TaskLifecycleAbandoned, "run-not-this-task", "")
	if err := read(foreign); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("fact naming a foreign run must fail closed: got %v", err)
	}

	uncorroborated := domain.TaskID("task-uncorroborated")
	seedTask(uncorroborated)
	addRun(uncorroborated, "run-unc")
	addFact(uncorroborated, 1, domain.TaskLifecycleStarted, "run-unc", "")
	addFact(uncorroborated, 2, domain.TaskLifecycleCompleted, "run-unc", "workunit-run-unc")
	if err := read(uncorroborated); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("completion with no recorded completion row must fail closed: got %v", err)
	}

	// A completion may name one of the task's own runs and a unit whose completion
	// exists globally, yet borrow a unit that belongs to another run/task. The
	// binding must resolve to a completion of the fact's own run (issue #1318 G3).
	other := domain.TaskID("task-other")
	seedTask(other)
	addRun(other, "run-other")
	addCompletion("run-other")
	borrow := domain.TaskID("task-borrow")
	seedTask(borrow)
	addRun(borrow, "run-borrow")
	addFact(borrow, 1, domain.TaskLifecycleStarted, "run-borrow", "")
	addFact(borrow, 2, domain.TaskLifecycleCompleted, "run-borrow", "workunit-run-other")
	if err := read(borrow); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("completion borrowing another run's unit must fail closed: got %v", err)
	}

	// A fact whose campaign does not match its run's persisted campaign is a
	// fabricated currency claim and fails closed (issue #1318 G3).
	fabricated := domain.TaskID("task-fabricated")
	seedTask(fabricated)
	addRun(fabricated, "run-fab") // run-fab's persisted campaign is c1.
	if _, err := db.ExecContext(ctx, `INSERT INTO task_lifecycle_facts (task_id, ordinal, kind, run_id, campaign_id, source_id, recorded_at)
		VALUES (?, 1, 'started', 'run-fab', 'c2', 'started', ?)`, fabricated, now); err != nil {
		t.Fatal(err)
	}
	if err := read(fabricated); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("fact with a fabricated campaign must fail closed: got %v", err)
	}

	// A started/completed fact whose campaign is cleared to NULL while its run has
	// a persisted campaign is also fabricated currency: two cleared facts would
	// compare equal as both-nil and release the slot. It fails closed (issue
	// #1318 H3).
	cleared := domain.TaskID("task-cleared")
	seedTask(cleared)
	addRun(cleared, "run-cleared") // run-cleared's persisted campaign is c1.
	if _, err := db.ExecContext(ctx, `INSERT INTO task_lifecycle_facts (task_id, ordinal, kind, run_id, campaign_id, source_id, recorded_at)
		VALUES (?, 1, 'started', 'run-cleared', NULL, 'started', ?)`, cleared, now); err != nil {
		t.Fatal(err)
	}
	if err := read(cleared); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("started fact with a cleared campaign must fail closed: got %v", err)
	}
}

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
	t.Parallel()
	ctx := t.Context()
	st := openTestStore(t)
	db := st.db
	_, seeded := SeedApprovedCampaign(t, st, "project-1", "implementation-run")
	var run domain.Run
	var task domain.Task
	if err := st.Write(ctx, func(tx *WriteTx) error {
		var err error
		run, err = tx.GetRun(ctx, seeded.ID)
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
