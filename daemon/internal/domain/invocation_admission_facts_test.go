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
	execution, _, _, _, _, _, _, _ := selectionClosure(t)
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
		{"empty host", func(f *domain.InvocationAdmissionFacts) { f.ExecutionHostID = new(domain.ExecutionHostID("")) }},
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
	t.Run("host on a review", func(t *testing.T) {
		t.Parallel()
		facts := domain.AdmittedFactsOfReview(review, nil)
		facts.ExecutionHostID = new(domain.DaemonHostID)
		if err := facts.Validate(); !errors.Is(err, domain.ErrAdmissionFactsInconsistent) {
			t.Fatalf("Validate = %v, want %v", err, domain.ErrAdmissionFactsInconsistent)
		}
	})
	t.Run("review without independence", func(t *testing.T) {
		t.Parallel()
		facts := domain.AdmittedFactsOfReview(review, nil)
		facts.Independence = nil
		if err := facts.Validate(); !errors.Is(err, domain.ErrAdmissionFactsInconsistent) {
			t.Fatalf("Validate = %v, want %v", err, domain.ErrAdmissionFactsInconsistent)
		}
	})
}

// TestAdmittedFactsHost pins where facts read a host from: an execution
// admission's recorded host, the daemon's host for one recorded before
// hosts existed, and null for a review and for an execution snapshot stored
// before facts carried a host.
func TestAdmittedFactsHost(t *testing.T) {
	t.Parallel()
	execution, _, _, _, _, _, _, _ := selectionClosure(t)
	review, _, _, _, _, _, _ := reviewAdmissionClosure(t)
	if execution.ExecutionHostID != "" {
		t.Fatalf("fixture admission records host %q, want none", execution.ExecutionHostID)
	}
	stored := domain.AdmittedFactsOfExecution(execution, domain.RoleImplementer)
	if stored.ExecutionHostID == nil || *stored.ExecutionHostID != domain.DaemonHostID {
		t.Fatalf("stored admission facts host = %v, want %q", stored.ExecutionHostID, domain.DaemonHostID)
	}
	hosted := execution
	hosted.ExecutionHostID = "mac-studio"
	if facts := domain.AdmittedFactsOfExecution(hosted, domain.RoleImplementer); facts.ExecutionHostID == nil ||
		*facts.ExecutionHostID != "mac-studio" {
		t.Fatalf("hosted admission facts host = %v, want mac-studio", facts.ExecutionHostID)
	}
	if facts := domain.AdmittedFactsOfReview(review, nil); facts.ExecutionHostID != nil {
		t.Fatalf("review facts host = %q, want null", *facts.ExecutionHostID)
	}
	snapshot := stored
	snapshot.ExecutionHostID = nil
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("execution facts without a host: Validate = %v", err)
	}
}

// TestExecutionFailureAdmittedFacts pins that a failure card's admitted facts
// are the failed invocation's execution admission, never another invocation's
// or a review's.
func TestExecutionFailureAdmittedFacts(t *testing.T) {
	t.Parallel()
	execution, _, _, _, _, _, _, _ := selectionClosure(t)
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
