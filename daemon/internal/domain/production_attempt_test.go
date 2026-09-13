package domain

import (
	"errors"
	"testing"
)

func validProductionAttempt() ProductionAttempt {
	return ProductionAttempt{
		CampaignID: "campaign-1", AttemptNumber: 2, Kind: ProductionAttemptRetry,
		Reason: "retry after repair", ParentRunID: "run-1",
		SourceDigest: "sha256:source", ApprovedSpecDigest: "sha256:approved",
		SpecificationRunID: "run-specification-1", ImplementationRunID: "run-2",
	}
}

func TestProductionAttemptValidation(t *testing.T) {
	t.Parallel()
	valid := validProductionAttempt()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*ProductionAttempt)
		want   error
	}{
		{"unknown kind", func(a *ProductionAttempt) { a.Kind = "other" }, ErrInvalidProductionAttemptKind},
		{"retry without reason", func(a *ProductionAttempt) { a.Reason = "" }, ErrProductionAttemptInconsistent},
		{"retry without parent", func(a *ProductionAttempt) { a.ParentRunID = "" }, ErrProductionAttemptInconsistent},
		{"retry without approved spec", func(a *ProductionAttempt) { a.ApprovedSpecDigest = "" }, ErrProductionAttemptInconsistent},
		{"noncontiguous first retry", func(a *ProductionAttempt) { a.AttemptNumber = 1 }, ErrProductionAttemptInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempt := valid
			tc.mutate(&attempt)
			if err := attempt.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestProductionAttemptRevisionLinkValidation(t *testing.T) {
	t.Parallel()
	valid := func() ProductionAttempt {
		revises := RunID("run-blocked")
		command := "cmd-answer"
		return ProductionAttempt{
			CampaignID: "campaign-2", AttemptNumber: 1, Kind: ProductionAttemptInitial,
			SourceDigest:       "sha256:source",
			SpecificationRunID: "run-specification-2", ImplementationRunID: "run-3",
			RevisesRunID: &revises, RevisionCommandID: &command,
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid revision attempt: %v", err)
	}
	empty := ""
	emptyRun := RunID("")
	sameAsImpl := RunID("run-3")
	cases := []struct {
		name   string
		mutate func(*ProductionAttempt)
		want   error
	}{
		{"revises without command", func(a *ProductionAttempt) { a.RevisionCommandID = nil }, ErrProductionAttemptInconsistent},
		{"command without revises", func(a *ProductionAttempt) { a.RevisesRunID = nil }, ErrProductionAttemptInconsistent},
		{"link on a retry", func(a *ProductionAttempt) {
			a.Kind = ProductionAttemptRetry
			a.AttemptNumber = 2
			a.Reason = "retry after repair"
			a.ParentRunID = "run-1"
			a.ApprovedSpecDigest = "sha256:approved"
		}, ErrProductionAttemptInconsistent},
		{"revises equals implementation run", func(a *ProductionAttempt) { a.RevisesRunID = &sameAsImpl }, ErrProductionAttemptInconsistent},
		{"empty revises", func(a *ProductionAttempt) { a.RevisesRunID = &emptyRun }, ErrEmptyID},
		{"empty command", func(a *ProductionAttempt) { a.RevisionCommandID = &empty }, ErrEmptyID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempt := valid()
			tc.mutate(&attempt)
			if err := attempt.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRunProductionLineageValidation(t *testing.T) {
	t.Parallel()
	run := Run{ID: "run-1", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
	run.CampaignID = "campaign-1"
	run.AttemptNumber = 2
	run.ParentRunID = "run-0"
	run.AttemptReason = "retry"
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
	run.AttemptReason = ""
	if err := run.Validate(); !errors.Is(err, ErrProductionAttemptInconsistent) {
		t.Fatalf("Validate() = %v, want ErrProductionAttemptInconsistent", err)
	}
}
