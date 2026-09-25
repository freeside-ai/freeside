package store

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
