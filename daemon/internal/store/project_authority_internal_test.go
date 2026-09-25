package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// recordProjectAdmission records one more attended admission, cloned from
// template, for a fresh run under projectID against base. Each run gets its
// own auth identity, so the template's identity capacity does not refuse it.
func recordProjectAdmission(
	t *testing.T, s *Store, template domain.ExecutionAdmission,
	runID domain.RunID, projectID domain.ProjectID, base domain.BaseRevision,
) (domain.ExecutionAdmission, error) {
	t.Helper()
	ctx := context.Background()
	stageID := domain.StageID("stage-" + string(runID))
	attemptID := domain.AttemptID("attempt-" + string(runID))
	invocationID := domain.InvocationID("inv-" + string(runID))
	identityID := domain.AuthIdentityID("auth-" + string(runID))
	run := domain.Run{
		ID: runID, ProjectID: projectID, SpecDigest: template.SpecDigest, PolicyDigest: template.PolicyDigest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		}},
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: template.Backend, Capabilities: template.Capabilities,
		OperatingMode: template.OperatingMode, CredentialMode: template.CredentialMode,
		EgressProfile: template.EgressProfile, ImageRef: template.ImageRef,
		SpecDigest: template.SpecDigest, PolicyDigest: template.PolicyDigest,
		InputDigest: template.InputDigest, Base: base, Workspace: template.Workspace,
		AuthIdentityID: &identityID, AdmittedAt: template.AdmittedAt,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
			ID: identityID, Provider: "claude", AuthStoreMutationLease: true,
			MaxParallelExecutions: 1,
			Interim:               domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
		}, template.AdmittedAt)
	}); err != nil {
		t.Fatalf("put run %q: %v", runID, err)
	}
	return admission, s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	})
}

func readProject(t *testing.T, s *Store, id domain.ProjectID) (domain.Project, error) {
	t.Helper()
	var project domain.Project
	err := s.Read(context.Background(), func(tx *ReadTx) error {
		var err error
		project, err = tx.GetProject(context.Background(), id)
		return err
	})
	return project, err
}

func countAdmissions(t *testing.T, s *Store, invocationID domain.InvocationID) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM execution_admissions WHERE invocation_id = ?`,
		invocationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestExecutionAdmissionRegistersProject pins the #1535 invariant: recording
// an admission binds the run's project to the admission's base repository, an
// exact replay converges, and a second project on the same repository gets
// its own row.
func TestExecutionAdmissionRegistersProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)

	want := domain.Project{ID: "proj-1", Repo: admission.Base.Repo, RepositoryID: admission.Base.RepositoryID}
	got, err := readProject(t, s, "proj-1")
	if err != nil {
		t.Fatalf("project after admission: %v", err)
	}
	if got != want {
		t.Fatalf("project = %+v, want %+v", got, want)
	}

	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("replay admission: %v", err)
	}

	if _, err := recordProjectAdmission(t, s, admission, "run-2", "proj-2", admission.Base); err != nil {
		t.Fatalf("second project on the same repository: %v", err)
	}
	second, err := readProject(t, s, "proj-2")
	if err != nil {
		t.Fatalf("second project: %v", err)
	}
	if second.Repo != admission.Base.Repo || second.RepositoryID != admission.Base.RepositoryID {
		t.Fatalf("second project = %+v, want %s (%d)", second, admission.Base.Repo, admission.Base.RepositoryID)
	}
}

// TestExecutionAdmissionReplayHealsMissingProject shows the replay path
// registers too, so a store that recorded the admission before #1535 gains
// its row on the next exact replay.
func TestExecutionAdmissionReplayHealsMissingProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects`); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("replay admission: %v", err)
	}
	if _, err := readProject(t, s, "proj-1"); err != nil {
		t.Fatalf("project after replay: %v", err)
	}
}

// TestExecutionAdmissionRefusesProjectRebinding: a daemon pointed at another
// repository cannot run a task whose project is bound elsewhere. The
// admission fails whole, so no work is admitted.
func TestExecutionAdmissionRefusesProjectRebinding(t *testing.T) {
	t.Parallel()
	s, admission := seedAdmission(t, nil)
	other := admission.Base
	other.Repo, other.RepositoryID = "owner/other", 515151

	rebinding, err := recordProjectAdmission(t, s, admission, "run-2", "proj-1", other)
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("rebinding admission err = %v, want ErrImmutableConflict", err)
	}
	if n := countAdmissions(t, s, rebinding.InvocationID); n != 0 {
		t.Fatalf("rebinding admission rows = %d, want 0", n)
	}
	got, err := readProject(t, s, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != admission.Base.Repo {
		t.Fatalf("project rebound to %q", got.Repo)
	}
}

// rerunProjectAuthorityBackfill rewinds the store to just before 0081 and
// migrates it again, so the data step runs over the store's current rows.
func rerunProjectAuthorityBackfill(t *testing.T, s *Store) error {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 81`); err != nil {
		t.Fatal(err)
	}
	return migrate(ctx, s.db, migrations.FS)
}

// TestProjectAuthorityBackfill covers migration 0081: a store with admissions
// and no projects rows gains them from the admissions, an existing row is left
// alone, and a project admitted against two repositories fails by name.
func TestProjectAuthorityBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("registers from admissions", func(t *testing.T) {
		t.Parallel()
		s, admission := seedAdmission(t, nil)
		other := admission.Base
		other.Repo, other.RepositoryID = "owner/other", 515151
		if _, err := recordProjectAdmission(t, s, admission, "run-2", "proj-2", other); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM projects`); err != nil {
			t.Fatal(err)
		}
		if err := rerunProjectAuthorityBackfill(t, s); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		for id, want := range map[domain.ProjectID]domain.BaseRevision{"proj-1": admission.Base, "proj-2": other} {
			got, err := readProject(t, s, id)
			if err != nil {
				t.Fatalf("project %q after backfill: %v", id, err)
			}
			if got.Repo != want.Repo || got.RepositoryID != want.RepositoryID {
				t.Fatalf("project %q = %+v, want %s (%d)", id, got, want.Repo, want.RepositoryID)
			}
		}
	})

	t.Run("skips an unreadable admission", func(t *testing.T) {
		t.Parallel()
		s, admission := seedAdmission(t, nil)
		// A second admission on the same run whose body does not
		// reconstruct contributes nothing; the readable one still binds.
		seedRawAdmission(t, ctx, s, admission)
		if _, err := s.db.ExecContext(ctx, `DELETE FROM projects`); err != nil {
			t.Fatal(err)
		}
		if err := rerunProjectAuthorityBackfill(t, s); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		got, err := readProject(t, s, "proj-1")
		if err != nil {
			t.Fatalf("project after backfill: %v", err)
		}
		if got.Repo != admission.Base.Repo || got.RepositoryID != admission.Base.RepositoryID {
			t.Fatalf("project = %+v, want %s (%d)", got, admission.Base.Repo, admission.Base.RepositoryID)
		}
	})

	t.Run("takes the project from the run body", func(t *testing.T) {
		t.Parallel()
		s, _ := seedAdmission(t, nil)
		// A copied project column that disagrees with the run body must not
		// mint a binding for either project. The task column moves with it,
		// as the runs_task_update trigger requires.
		for _, stmt := range []string{
			`DELETE FROM projects`,
			`UPDATE tasks SET project_id = 'proj-forged'`,
			`UPDATE runs SET project_id = 'proj-forged'`,
		} {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
		if err := rerunProjectAuthorityBackfill(t, s); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("backfill registered %d projects from a divergent run row, want 0", count)
		}
	})

	t.Run("leaves an existing row", func(t *testing.T) {
		t.Parallel()
		s, _ := seedAdmission(t, nil)
		if _, err := s.db.ExecContext(ctx, `DELETE FROM projects`); err != nil {
			t.Fatal(err)
		}
		existing := domain.Project{ID: "proj-1", Repo: "owner/elsewhere", RepositoryID: 616161}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.RegisterProject(ctx, existing)
		}); err != nil {
			t.Fatal(err)
		}
		if err := rerunProjectAuthorityBackfill(t, s); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		got, err := readProject(t, s, "proj-1")
		if err != nil {
			t.Fatal(err)
		}
		if got != existing {
			t.Fatalf("project = %+v, want untouched %+v", got, existing)
		}
	})

	t.Run("conflicting admissions fail by name", func(t *testing.T) {
		t.Parallel()
		s, admission := seedAdmission(t, nil)
		// Each admission registers its own binding, so drop the row between
		// them to reach the contradiction the backfill must refuse.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM projects`); err != nil {
			t.Fatal(err)
		}
		other := admission.Base
		other.Repo, other.RepositoryID = "owner/other", 515151
		if _, err := recordProjectAdmission(t, s, admission, "run-2", "proj-1", other); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM projects`); err != nil {
			t.Fatal(err)
		}
		err := rerunProjectAuthorityBackfill(t, s)
		if !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("backfill err = %v, want ErrImmutableConflict", err)
		}
		if want := `project "proj-1"`; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("backfill err = %v, want it to name %s", err, want)
		}
		if _, err := readProject(t, s, "proj-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("project after failed backfill: %v, want ErrNotFound", err)
		}
	})
}

// TestClosableSourceReportsMissingProject: after #1535 a missing project on
// the closure path is corruption, reported as ErrProjectAuthorityMissing and
// not as ErrNotFound, which the closure gate would read as "not closable".
func TestClosableSourceReportsMissingProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTemplateStore(t, Options{})
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.closableSource(ctx, domain.WorkUnitDeclaration{ProjectID: "proj-missing", RunID: "run-1"})
		return err
	})
	if !errors.Is(err, ErrProjectAuthorityMissing) {
		t.Fatalf("closable source err = %v, want ErrProjectAuthorityMissing", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("closable source err = %v wraps ErrNotFound", err)
	}
}
