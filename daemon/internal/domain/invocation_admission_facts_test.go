package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestAdmittedFactsOfReviewIndependence pins the review facts' independence:
// one entry per writing role in domain.WritingRoles order, compared against
// the lineage each writer's admission recorded, and unknown when either side
// recorded none.
func TestAdmittedFactsOfReviewIndependence(t *testing.T) {
	t.Parallel()
	review, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	review.LineageGroup = "openai"
	cases := []struct {
		name    string
		review  string
		writers map[domain.RoleName]string
		want    []domain.LineageRelation
	}{
		{
			"matched and differed", "openai",
			map[domain.RoleName]string{domain.RoleImplementer: "openai", domain.RoleRemediator: "anthropic"},
			[]domain.LineageRelation{domain.LineageMatched, domain.LineageDiffered},
		},
		{
			"no writer admissions", "openai", nil,
			[]domain.LineageRelation{domain.LineageUnknown, domain.LineageUnknown},
		},
		{
			"writer without a recorded lineage", "openai",
			map[domain.RoleName]string{domain.RoleImplementer: ""},
			[]domain.LineageRelation{domain.LineageUnknown, domain.LineageUnknown},
		},
		{
			"reviewer of unknown lineage", "",
			map[domain.RoleName]string{domain.RoleImplementer: "openai"},
			[]domain.LineageRelation{domain.LineageUnknown, domain.LineageUnknown},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			admission := review
			admission.LineageGroup = tc.review
			facts := domain.AdmittedFactsOfReview(admission, tc.writers)
			if err := facts.Validate(); err != nil {
				t.Fatal(err)
			}
			if len(facts.Independence) != len(domain.WritingRoles) {
				t.Fatalf("independence = %+v, want one entry per writing role", facts.Independence)
			}
			for i, entry := range facts.Independence {
				if entry.WritingRole != domain.WritingRoles[i] || entry.Lineage != tc.want[i] {
					t.Fatalf("independence[%d] = %+v, want %s %s", i, entry, domain.WritingRoles[i], tc.want[i])
				}
			}
			if (facts.LineageGroup == nil) != (tc.review == "") {
				t.Fatalf("lineage_group = %v for recorded %q", facts.LineageGroup, tc.review)
			}
		})
	}
}

// TestInvocationAdmissionFactsValidate pins the shapes no admission record
// could have produced, each refused.
func TestInvocationAdmissionFactsValidate(t *testing.T) {
	t.Parallel()
	execution, _, _, _, _, _, _, _ := derivationClosure(t)
	review, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	for _, kind := range domain.AllAdmissionKinds {
		facts := domain.AdmittedFactsOfExecution(execution, domain.RoleImplementer)
		if kind == domain.AdmissionKindReview {
			facts = domain.AdmittedFactsOfReview(review, nil)
		}
		if err := facts.Validate(); err != nil {
			t.Fatalf("%s facts = %v", kind, err)
		}
	}
	bogusRole := domain.RoleName("auditor-of-auditors")
	bogusHarness := domain.HarnessClientKind("pi")
	cases := []struct {
		name   string
		mutate func(*domain.InvocationAdmissionFacts)
	}{
		{"unknown kind", func(f *domain.InvocationAdmissionFacts) { f.Kind = "inference" }},
		{"unknown role", func(f *domain.InvocationAdmissionFacts) { f.Role = &bogusRole }},
		{"unknown harness", func(f *domain.InvocationAdmissionFacts) { f.Harness = &bogusHarness }},
		{"no admission instant", func(f *domain.InvocationAdmissionFacts) { f.AdmittedAt = time.Time{} }},
		{"admission instant not UTC", func(f *domain.InvocationAdmissionFacts) {
			f.AdmittedAt = f.AdmittedAt.In(time.FixedZone("east", 3600))
		}},
		{"independence on a writer", func(f *domain.InvocationAdmissionFacts) {
			f.Independence = []domain.IndependenceEntry{{WritingRole: domain.RoleImplementer, Lineage: domain.LineageMatched}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := domain.AdmittedFactsOfExecution(execution, domain.RoleImplementer)
			tc.mutate(&facts)
			if err := facts.Validate(); !errors.Is(err, domain.ErrAdmissionFactsInconsistent) {
				t.Fatalf("Validate = %v, want %v", err, domain.ErrAdmissionFactsInconsistent)
			}
		})
	}
	t.Run("review without independence", func(t *testing.T) {
		t.Parallel()
		facts := domain.AdmittedFactsOfReview(review, nil)
		facts.Independence = nil
		if err := facts.Validate(); !errors.Is(err, domain.ErrAdmissionFactsInconsistent) {
			t.Fatalf("Validate = %v, want %v", err, domain.ErrAdmissionFactsInconsistent)
		}
	})
}

// TestExecutionFailureAdmittedFacts pins that a failure card's admitted facts
// are the failed invocation's execution admission, never another invocation's
// or a review's.
func TestExecutionFailureAdmittedFacts(t *testing.T) {
	t.Parallel()
	execution, _, _, _, _, _, _, _ := derivationClosure(t)
	review, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	writer := domain.AdmittedFactsOfExecution(execution, domain.RoleImplementer)
	failure := domain.ExecutionFailureFacts{
		Outcome: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseQuota,
		Stage: domain.StageNameImplementation, InvocationID: execution.InvocationID, Admitted: &writer,
	}
	if err := failure.Validate(); err != nil {
		t.Fatalf("admitted facts of the failed invocation = %v", err)
	}
	other := failure
	other.InvocationID = "inv-other"
	if err := other.Validate(); !errors.Is(err, domain.ErrCardFactInconsistent) {
		t.Fatalf("another invocation's facts = %v, want %v", err, domain.ErrCardFactInconsistent)
	}
	reviewer := domain.AdmittedFactsOfReview(review, nil)
	reviewer.InvocationID = execution.InvocationID
	ofReview := failure
	ofReview.Admitted = &reviewer
	if err := ofReview.Validate(); !errors.Is(err, domain.ErrCardFactInconsistent) {
		t.Fatalf("a review's facts = %v, want %v", err, domain.ErrCardFactInconsistent)
	}
}
