package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validRun() domain.Run {
	return domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: "stage-1", RunID: "run-1", Name: "implementation",
			Attempts: []domain.Attempt{{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}},
		}},
	}
}

// TestRunValidate covers the run/stage/attempt required scope and join keys and
// the parent-key cross-checks: a run names its project and policy, and a child
// must both be well-formed and name its enclosing parent.
func TestRunValidate(t *testing.T) {
	if err := validRun().Validate(); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*domain.Run)
		wantErr error
	}{
		{"missing project", func(r *domain.Run) { r.ProjectID = "" }, domain.ErrEmptyID},
		{"missing policy digest", func(r *domain.Run) { r.PolicyDigest = "" }, domain.ErrEmptyField},
		{"missing spec digest", func(r *domain.Run) { r.SpecDigest = "" }, domain.ErrEmptyField},
		{"stage missing run_id", func(r *domain.Run) { r.Stages[0].RunID = "" }, domain.ErrEmptyID},
		{"stage run_id mismatch", func(r *domain.Run) { r.Stages[0].RunID = "run-other" }, domain.ErrParentKeyMismatch},
		{"attempt missing stage_id", func(r *domain.Run) { r.Stages[0].Attempts[0].StageID = "" }, domain.ErrEmptyID},
		{"attempt stage_id mismatch", func(r *domain.Run) { r.Stages[0].Attempts[0].StageID = "stage-other" }, domain.ErrParentKeyMismatch},
		{"attempt non-positive number", func(r *domain.Run) { r.Stages[0].Attempts[0].Number = 0 }, domain.ErrNonPositive},
		{"duplicate stage id", func(r *domain.Run) {
			r.Stages = append(r.Stages, r.Stages[0])
		}, domain.ErrDuplicate},
		{"duplicate attempt id", func(r *domain.Run) {
			a := r.Stages[0].Attempts[0]
			a.Number, a.InvocationID = 2, "inv-2"
			r.Stages[0].Attempts = append(r.Stages[0].Attempts, a)
		}, domain.ErrDuplicate},
		{"attempt numbers out of order", func(r *domain.Run) {
			r.Stages[0].Attempts = []domain.Attempt{
				{ID: "a2", StageID: "stage-1", Number: 2, InvocationID: "inv-2"},
				{ID: "a1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"},
			}
		}, domain.ErrNonContiguous},
		{"attempt numbers non-contiguous", func(r *domain.Run) {
			r.Stages[0].Attempts = []domain.Attempt{
				{ID: "a1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"},
				{ID: "a3", StageID: "stage-1", Number: 3, InvocationID: "inv-3"},
			}
		}, domain.ErrNonContiguous},
		{"duplicate invocation id", func(r *domain.Run) {
			a := r.Stages[0].Attempts[0]
			a.ID, a.Number = "attempt-2", 2
			r.Stages[0].Attempts = append(r.Stages[0].Attempts, a)
		}, domain.ErrDuplicate},
		{"invocation id reused across stages", func(r *domain.Run) {
			r.Stages = append(r.Stages, domain.Stage{
				ID: "stage-2", RunID: "run-1", Name: "review",
				Attempts: []domain.Attempt{{ID: "attempt-2", StageID: "stage-2", Number: 1, InvocationID: "inv-1"}},
			})
		}, domain.ErrDuplicate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validRun()
			tt.mutate(&r)
			if err := r.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
