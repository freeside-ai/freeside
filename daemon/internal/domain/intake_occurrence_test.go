package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var intakeTS = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// admittedOccurrence returns a valid, fully-bound occurrence the negative
// tests mutate one field of. Its admission key is derived from its own
// coordinates, as a real admission would set it.
func admittedOccurrence(t *testing.T) domain.IntakeOccurrence {
	t.Helper()
	o := domain.IntakeOccurrence{
		Repo: "owner/repo", RepositoryID: 42, IssueNumber: 7, Label: "freeside",
		Ordinal: 2, State: domain.IntakeOccurrenceAbsent,
		Admission: &domain.IntakeAdmission{
			ProposalInstanceID: "proposal-1",
			ProposalDigest:     domain.Digest(contentaddr.Sum([]byte("p"))),
			Subject: domain.IntakeSubjectBinding{
				ProjectID: "proj-1", SpecificationRunID: "run-spec-1",
				WorkUnitID:           domain.WorkUnitIDForRun("run-spec-1"),
				PolicyArtifactID:     "policy-art-1",
				PolicyArtifactDigest: domain.Digest(contentaddr.Sum([]byte("rp"))),
				ResolvedPolicyDigest: domain.Digest(contentaddr.Sum([]byte("rp"))),
				Source: domain.SpecificationSource{
					Kind:         domain.SpecificationSourceIssueSubject,
					IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 42, IssueNumber: 7},
				},
			},
		},
		RecordedAt: intakeTS,
	}
	o.Admission.AdmissionKey = o.ProposalAdmissionKey()
	if err := o.Validate(); err != nil {
		t.Fatalf("baseline admitted occurrence is not valid: %v", err)
	}
	return o
}

func TestIntakeOccurrenceValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		want error
		mut  func(o *domain.IntakeOccurrence)
	}{
		{"empty repo", domain.ErrEmptyField, func(o *domain.IntakeOccurrence) { o.Repo = "" }},
		{"non-positive repository id", domain.ErrNonPositive, func(o *domain.IntakeOccurrence) { o.RepositoryID = 0 }},
		{"non-positive issue", domain.ErrNonPositive, func(o *domain.IntakeOccurrence) { o.IssueNumber = 0 }},
		{"empty label", domain.ErrEmptyField, func(o *domain.IntakeOccurrence) { o.Label = "" }},
		{"non-positive ordinal", domain.ErrNonPositive, func(o *domain.IntakeOccurrence) { o.Ordinal = 0 }},
		{"invalid state", domain.ErrInvalidIntakeOccurrenceState, func(o *domain.IntakeOccurrence) { o.State = "gone" }},
		{"non-utc recorded_at", domain.ErrTimestampNotUTC, func(o *domain.IntakeOccurrence) {
			o.RecordedAt = intakeTS.In(time.FixedZone("x", 3600))
		}},
		{"tampered admission key", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			// A key derived from a different ordinal points at a foreign
			// proposal; the occurrence must reject it.
			foreign := *o
			foreign.Ordinal = 99
			o.Admission.AdmissionKey = foreign.ProposalAdmissionKey()
		}},
		{"foreign issue subject", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			o.Admission.Subject.Source.IssueSubject.IssueNumber = 8
		}},
		{"stale policy digest", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			o.Admission.Subject.PolicyArtifactDigest = "sha256:not-canonical"
		}},
		{"work unit not derived", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			o.Admission.Subject.WorkUnitID = "workunit-foreign"
		}},
		{"policy artifact digest differs from resolved", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			o.Admission.Subject.PolicyArtifactDigest = domain.Digest(contentaddr.Sum([]byte("other")))
		}},
		{"supersession without admission", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			o.Admission = nil
			o.Supersession = &domain.IntakeSupersession{Reason: domain.IntakeSupersededIssueClosed, RecordedAt: intakeTS}
		}},
		{"supersession while present", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			o.State = domain.IntakeOccurrencePresent
			o.Supersession = &domain.IntakeSupersession{Reason: domain.IntakeSupersededLabelRemoved, RecordedAt: intakeTS}
		}},
		{"bad refusal reason", domain.ErrInvalidIntakeStartRefusalReason, func(o *domain.IntakeOccurrence) {
			o.Refusal = &domain.IntakeStartRefusal{Reason: "nope", RecordedAt: intakeTS}
		}},
		{"admission subject not issue_subject", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			// A label occurrence's subject must be the issue_subject arm; a
			// work_item_artifact source would name an arbitrary artifact as authority.
			o.Admission.Subject.Source = domain.SpecificationSource{
				Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: "spec-art-1",
			}
		}},
		{"refusal without admission", domain.ErrIntakeOccurrenceInconsistent, func(o *domain.IntakeOccurrence) {
			o.Admission = nil
			o.Refusal = &domain.IntakeStartRefusal{Reason: domain.IntakeRefusalWIPCapExhausted, RecordedAt: intakeTS}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := admittedOccurrence(t)
			tc.mut(&o)
			err := o.Validate()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestIntakeOccurrenceKeyDerivation is the admission-key replay contract: the
// same occurrence always derives the same key, and a distinct ordinal derives
// a distinct key, so a retry converges and a new occurrence does not collide.
func TestIntakeOccurrenceKeyDerivation(t *testing.T) {
	o := admittedOccurrence(t)
	first, err := o.ProposalAdmissionKey().String()
	if err != nil {
		t.Fatal(err)
	}
	again, err := o.ProposalAdmissionKey().String()
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("same occurrence derived %q then %q", first, again)
	}
	next := o
	next.Ordinal = 3
	other, err := next.ProposalAdmissionKey().String()
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatalf("ordinal 2 and 3 collide on %q", first)
	}
	if want := "label-intake/42/7/freeside/2"; o.UpstreamEventID() != want {
		t.Fatalf("UpstreamEventID() = %q, want %q", o.UpstreamEventID(), want)
	}
}

// TestIntakeOccurrenceTransitions pins the per-occurrence state machine: absent
// and closed never return to present, closed is terminal, and same-state
// re-observation is idempotent.
func TestIntakeOccurrenceTransitions(t *testing.T) {
	allow := map[domain.IntakeOccurrenceState][]domain.IntakeOccurrenceState{
		domain.IntakeOccurrencePresent: {domain.IntakeOccurrencePresent, domain.IntakeOccurrenceAbsent, domain.IntakeOccurrenceClosed},
		domain.IntakeOccurrenceAbsent:  {domain.IntakeOccurrenceAbsent, domain.IntakeOccurrenceClosed},
		domain.IntakeOccurrenceClosed:  {domain.IntakeOccurrenceClosed},
	}
	for _, from := range domain.AllIntakeOccurrenceStates {
		permitted := map[domain.IntakeOccurrenceState]bool{}
		for _, to := range allow[from] {
			permitted[to] = true
		}
		for _, to := range domain.AllIntakeOccurrenceStates {
			if got := from.CanTransitionTo(to); got != permitted[to] {
				t.Errorf("%s -> %s = %v, want %v", from, to, got, permitted[to])
			}
		}
	}
}

func TestSpecificationSourceValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		want error
		src  domain.SpecificationSource
	}{
		{"empty kind", domain.ErrInvalidSpecificationSourceKind, domain.SpecificationSource{}},
		{"spec arm missing id", domain.ErrEmptyID, domain.SpecificationSource{Kind: domain.SpecificationSourceWorkItemArtifact}},
		{"spec arm with issue", domain.ErrSpecificationSourceInconsistent, domain.SpecificationSource{
			Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: "a",
			IssueSubject: &domain.IssueSubjectRef{Repo: "r", RepositoryID: 1, IssueNumber: 1},
		}},
		{"issue arm with work item", domain.ErrSpecificationSourceInconsistent, domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject, WorkItemArtifactID: "a",
			IssueSubject: &domain.IssueSubjectRef{Repo: "r", RepositoryID: 1, IssueNumber: 1},
		}},
		{"issue arm missing subject", domain.ErrSpecificationSourceInconsistent, domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject,
		}},
		{"issue arm bad ref", domain.ErrNonPositive, domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &domain.IssueSubjectRef{Repo: "r", RepositoryID: 0, IssueNumber: 1},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.src.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
