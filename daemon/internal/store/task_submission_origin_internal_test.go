package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

const (
	originTestRepositoryID = 1278475858
	originTestRepository   = "freeasinbird/gh-imgup"
)

// seedOriginTestTarget registers the project a daemon-origin submission's
// refusal must name and the task and specification run its row references.
func seedOriginTestTarget(t *testing.T, ctx context.Context, st *Store) (domain.TaskID, domain.RunID) {
	t.Helper()
	var taskID domain.TaskID
	runID := domain.RunID("run-origin-1")
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{
			ID: "proj-1", Repo: originTestRepository, RepositoryID: originTestRepositoryID,
		}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{
				Repo: originTestRepository, RepositoryID: originTestRepositoryID, IssueNumber: 42,
			},
		})
		if err != nil {
			return err
		}
		taskID = task.ID
		return tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "proj-1", TaskID: taskID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		})
	}); err != nil {
		t.Fatal(err)
	}
	return taskID, runID
}

func originTestRefusal(
	t *testing.T, class domain.ImageUpkeepRefusalClass, repositoryID int64,
) domain.ImageUpkeepRefusal {
	t.Helper()
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: repositoryID, Repository: originTestRepository,
		Harness: domain.HarnessClientClaudeCode, TipSHA: strings.Repeat("e", 40),
		MeasuredFromImageID: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		Class:               class,
		Findings: []domain.ImageUpkeepClauseFinding{{
			Clause: domain.RebuildRefusalLockfileInconsistent, Detail: "lockfile root disagrees",
		}},
		RecordedAt: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

// TestDaemonSubmissionRequiresRepairRefusal: only project-image upkeep
// submits a daemon-origin task, and only for a repair-class refusal on the
// project's own repository, so the store refuses any other daemon-origin
// write and fails closed on a stored one whose refusal no longer backs it.
func TestDaemonSubmissionRequiresRepairRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openProjectImageStore(t)
	taskID, runID := seedOriginTestTarget(t, ctx, st)
	repair := originTestRefusal(t, domain.ImageUpkeepRefusalRepair, originTestRepositoryID)
	policy := originTestRefusal(t, domain.ImageUpkeepRefusalPolicy, originTestRepositoryID)
	foreign := originTestRefusal(t, domain.ImageUpkeepRefusalRepair, 42)
	submission := func(source domain.Digest) domain.TaskSubmission {
		return domain.TaskSubmission{
			CommandID: "daemon-cmd-1", Origin: domain.TaskSubmissionOriginDaemon, ProjectID: "proj-1",
			SourceDigest: source, TaskID: taskID, SpecificationRunID: runID,
			Name: domain.DisplayName{Text: "Repair the npm lockfile", Source: domain.DisplayNameSourceName},
		}
	}
	put := func(s domain.TaskSubmission) error {
		return st.Write(ctx, func(tx *WriteTx) error { return tx.PutTaskSubmission(ctx, s) })
	}

	if err := put(submission(repair.ID)); !errors.Is(err, ErrDaemonSubmissionUnsourced) {
		t.Fatalf("unrecorded refusal: put = %v, want ErrDaemonSubmissionUnsourced", err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		for _, refusal := range []domain.ImageUpkeepRefusal{repair, policy, foreign} {
			if _, err := tx.RecordImageUpkeepRefusal(ctx, refusal); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]domain.Digest{
		"policy-class refusal":      policy.ID,
		"another repository's":      foreign.ID,
		"digest that is no refusal": domain.Digest("sha256:" + strings.Repeat("0", 64)),
	} {
		if err := put(submission(source)); !errors.Is(err, ErrDaemonSubmissionUnsourced) {
			t.Fatalf("%s: put = %v, want ErrDaemonSubmissionUnsourced", name, err)
		}
	}

	want := submission(repair.ID)
	if err := put(want); err != nil {
		t.Fatalf("repair refusal: put = %v", err)
	}
	read := func() (domain.TaskSubmission, error) {
		var got domain.TaskSubmission
		err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			got, _, err = tx.GetTaskSubmissionSnapshot(ctx, want.CommandID)
			return err
		})
		return got, err
	}
	got, err := read()
	if err != nil || got.Origin != domain.TaskSubmissionOriginDaemon || got.DeviceID != nil ||
		got.SourceDigest != repair.ID {
		t.Fatalf("read = %+v err=%v, want the daemon-origin submission", got, err)
	}

	// The refusal row is the authority, re-read on decode: a stored row whose
	// refusal is gone reads as inconsistent, not as a submission.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM image_upkeep_refusals WHERE id = ?`, repair.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); !errors.Is(err, ErrRowInconsistent) || !errors.Is(err, ErrDaemonSubmissionUnsourced) {
		t.Fatalf("read without its refusal: err = %v, want ErrRowInconsistent", err)
	}
}

// TestTaskSubmissionOriginMigrationKeepsDeviceRows: a submission recorded
// before 0109 survives the table rebuild as a device submission, and its body,
// which names no origin, decodes as one.
func TestTaskSubmissionOriginMigrationKeepsDeviceRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/task-submission-origin.db", Options{})
	taskID, runID := seedOriginTestTarget(t, ctx, st)
	body := `{"command_id":"cmd-old","device_id":"device-1","project_id":"proj-1",` +
		`"source_digest":"sha256:` + strings.Repeat("1", 64) + `","task_id":"` + string(taskID) + `",` +
		`"specification_run_id":"` + string(runID) + `","name":{"text":"Old task","source":"operator"}}`

	// Return the table to the shape a daemon at 0108 holds, then record a
	// submission the way that daemon did.
	rewindUsagePools(t, ctx, st.db)
	for _, statement := range []string{
		`DROP TABLE task_line_changes`,
		`DELETE FROM schema_migrations WHERE name = '0113_task_line_changes.sql'`,
		`ALTER TABLE task_lines DROP COLUMN provenance`,
		`DELETE FROM schema_migrations WHERE name = '0111_task_line_provenance.sql'`,
		`DROP TABLE image_policy_adoptions`,
		`DELETE FROM schema_migrations WHERE name = '0110_image_policy_adoption.sql'`,
		`DROP TABLE task_submission_commands`,
		`CREATE TABLE task_submission_commands (
			command_id           TEXT PRIMARY KEY,
			device_id            TEXT    NOT NULL,
			project_id           TEXT    NOT NULL,
			task_id              TEXT    NOT NULL REFERENCES tasks (id),
			specification_run_id TEXT    NOT NULL REFERENCES runs (id),
			entity_version       INTEGER NOT NULL,
			as_of_revision       INTEGER NOT NULL,
			body                 TEXT    NOT NULL,
			request_digest       TEXT
		) STRICT`,
		`DELETE FROM schema_migrations WHERE name = '0109_task_submission_origin.sql'`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO task_submission_commands
		(command_id, device_id, project_id, task_id, specification_run_id, entity_version, as_of_revision, body)
		VALUES ('cmd-old', 'device-1', 'proj-1', ?, ?, 1, 1, ?)`, taskID, runID, body); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)

	var (
		got    domain.TaskSubmission
		origin string
	)
	if err := st.db.QueryRowContext(ctx,
		`SELECT origin FROM task_submission_commands WHERE command_id = 'cmd-old'`).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetTaskSubmission(ctx, "cmd-old")
		return err
	}); err != nil {
		t.Fatalf("read after migration: %v", err)
	}
	if origin != "device" || got.Origin != domain.TaskSubmissionOriginDevice ||
		got.DeviceID == nil || *got.DeviceID != "device-1" {
		t.Fatalf("after migration: column %q, submission %+v; want a device submission", origin, got)
	}
}
