package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDecideFailureDisposition(t *testing.T) {
	t.Parallel()
	failed := domain.FailureFacts{Route: "route-openai", UsagePool: "pool-a"}
	with := func(alternates ...domain.FailureAlternate) domain.FailureFacts {
		facts := failed
		facts.Alternates = alternates
		return facts
	}
	otherPool := domain.FailureAlternate{
		Agent: "sha256:alternate", Route: "route-openai", UsagePool: "pool-b", GenerationValid: true,
	}
	samePool := domain.FailureAlternate{
		Agent: "sha256:alternate", Route: "route-openai", UsagePool: "pool-a", GenerationValid: true,
	}
	otherRoute := domain.FailureAlternate{
		Agent: "sha256:alternate", Route: "route-anthropic", UsagePool: "pool-a", GenerationValid: true,
	}
	invalidGeneration := domain.FailureAlternate{
		Agent: "sha256:alternate", Route: "route-anthropic", UsagePool: "pool-b", GenerationValid: false,
	}
	unattributed := domain.FailureAlternate{Agent: "sha256:alternate", GenerationValid: true}

	for _, tc := range []struct {
		name  string
		cause domain.FailureCause
		facts domain.FailureFacts
		want  domain.FailureDisposition
	}{
		{
			"quota with an alternate on another usage pool", domain.FailureCauseQuota,
			with(otherPool), domain.FailureDispositionAlternateAgentEligible,
		},
		// Revision 39: two enrollments drawing on one usage pool share its
		// quota, so the second is no escape.
		{
			"quota with an alternate on the same usage pool", domain.FailureCauseQuota,
			with(samePool), domain.FailureDispositionWait,
		},
		{
			"quota with an alternate naming no usage pool", domain.FailureCauseQuota,
			with(unattributed), domain.FailureDispositionWait,
		},
		// An unrecorded failed-side pool or route proves nothing escapes it.
		{
			"quota with no recorded usage pool", domain.FailureCauseQuota,
			domain.FailureFacts{Route: "route-openai", Alternates: []domain.FailureAlternate{otherPool}},
			domain.FailureDispositionWait,
		},
		{
			"capacity with no recorded route", domain.FailureCauseServiceCapacity,
			domain.FailureFacts{UsagePool: "pool-a", Alternates: []domain.FailureAlternate{otherRoute}},
			domain.FailureDispositionWait,
		},
		{
			"quota with no alternate", domain.FailureCauseQuota,
			failed, domain.FailureDispositionWait,
		},
		{
			"expired credential with a valid alternate generation", domain.FailureCauseCredentialExpired,
			with(invalidGeneration, samePool), domain.FailureDispositionAlternateAgentEligible,
		},
		{
			"expired credential with no valid alternate generation", domain.FailureCauseCredentialExpired,
			with(invalidGeneration), domain.FailureDispositionAttention,
		},
		{
			"revoked credential with a valid alternate generation", domain.FailureCauseCredentialRevoked,
			with(samePool), domain.FailureDispositionAlternateAgentEligible,
		},
		{
			"revoked credential with no valid alternate generation", domain.FailureCauseCredentialRevoked,
			with(invalidGeneration), domain.FailureDispositionAttention,
		},
		{
			"capacity with an alternate on another route", domain.FailureCauseServiceCapacity,
			with(otherRoute), domain.FailureDispositionAlternateAgentEligible,
		},
		{
			"capacity with an alternate on the same route", domain.FailureCauseServiceCapacity,
			with(otherPool), domain.FailureDispositionWait,
		},
		{
			"capacity with an alternate naming no route", domain.FailureCauseServiceCapacity,
			with(unattributed), domain.FailureDispositionWait,
		},
		{
			"provider unavailable", domain.FailureCauseProviderUnavailable,
			with(otherRoute), domain.FailureDispositionRetrySameAgent,
		},
		{
			"invalid configuration", domain.FailureCauseInvalidConfiguration,
			with(otherPool, otherRoute), domain.FailureDispositionAttention,
		},
		{
			"unknown", domain.FailureCauseUnknown,
			with(otherPool, otherRoute), domain.FailureDispositionAttention,
		},
		{
			"contract contradiction", domain.FailureCauseContractContradiction,
			with(otherPool, otherRoute), domain.FailureDispositionFailLoud,
		},
		{
			"invalid zero cause", "",
			with(otherPool, otherRoute), domain.FailureDispositionAttention,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.DecideFailureDisposition(tc.cause, tc.facts); got != tc.want {
				t.Fatalf("DecideFailureDisposition(%q) = %q, want %q", tc.cause, got, tc.want)
			}
		})
	}
}

func TestDecideFailureDispositionIsTotal(t *testing.T) {
	t.Parallel()
	for _, cause := range domain.AllFailureCauses {
		got := domain.DecideFailureDisposition(cause, domain.FailureFacts{})
		if !slices.Contains(domain.AllFailureDispositions, got) {
			t.Errorf("DecideFailureDisposition(%q, empty facts) = %q, not a registered disposition", cause, got)
		}
	}
}

func TestReviewFailureClassFailureCause(t *testing.T) {
	t.Parallel()
	want := map[domain.ReviewFailureClass]domain.FailureCause{
		domain.ReviewFailureTransient:     domain.FailureCauseProviderUnavailable,
		domain.ReviewFailureConfiguration: domain.FailureCauseInvalidConfiguration,
		domain.ReviewFailureQuota:         domain.FailureCauseQuota,
		domain.ReviewFailureContradiction: domain.FailureCauseContractContradiction,
	}
	if len(want) != len(domain.AllReviewFailureClasses) {
		t.Fatalf("pinned %d classes, registered %d", len(want), len(domain.AllReviewFailureClasses))
	}
	for _, class := range domain.AllReviewFailureClasses {
		if got := class.FailureCause(); got != want[class] {
			t.Errorf("%q.FailureCause() = %q, want %q", class, got, want[class])
		}
	}
	if got := domain.ReviewFailureClass("").FailureCause(); got != domain.FailureCauseUnknown {
		t.Errorf("invalid class FailureCause() = %q, want unknown", got)
	}
}

func TestFailureCauseValidate(t *testing.T) {
	t.Parallel()
	for _, cause := range domain.AllFailureCauses {
		if err := cause.Validate(); err != nil {
			t.Errorf("%q.Validate() = %v", cause, err)
		}
	}
	for _, cause := range []domain.FailureCause{"", "transient"} {
		if err := cause.Validate(); !errors.Is(err, domain.ErrInvalidFailureCause) {
			t.Errorf("%q.Validate() = %v, want ErrInvalidFailureCause", cause, err)
		}
	}
}

func TestExecutionOutcomeCause(t *testing.T) {
	t.Parallel()
	outcome := func(status domain.ExecutionOutcomeStatus, cause domain.FailureCause) domain.ExecutionOutcome {
		return domain.ExecutionOutcome{
			InvocationID: "inv-1", AdmissionID: "sha256:admission",
			Status: status, Cause: cause,
			RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}
	}
	for _, tc := range []struct {
		name    string
		outcome domain.ExecutionOutcome
		want    error
	}{
		{"failed with a cause", outcome(domain.ExecutionOutcomeFailed, domain.FailureCauseQuota), nil},
		{"failed with unknown", outcome(domain.ExecutionOutcomeFailed, domain.FailureCauseUnknown), nil},
		{"failed without a cause", outcome(domain.ExecutionOutcomeFailed, ""), domain.ErrInvalidFailureCause},
		{"failed with an invalid cause", outcome(domain.ExecutionOutcomeFailed, "transient"), domain.ErrInvalidFailureCause},
		{"canceled without a cause", outcome(domain.ExecutionOutcomeCanceled, ""), nil},
		{"canceled with a cause", outcome(domain.ExecutionOutcomeCanceled, domain.FailureCauseUnknown), domain.ErrOutcomeInconsistent},
		{"lost with a cause", outcome(domain.ExecutionOutcomeLost, domain.FailureCauseQuota), domain.ErrOutcomeInconsistent},
		{"blocked with a cause", outcome(domain.ExecutionOutcomeBlocked, domain.FailureCauseQuota), domain.ErrOutcomeInconsistent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.outcome.Validate()
			if tc.want == nil && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestExecutionOutcomeCanonicalizeStoredRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status domain.ExecutionOutcomeStatus
		cause  domain.FailureCause
		want   domain.FailureCause
	}{
		{domain.ExecutionOutcomeFailed, "", domain.FailureCauseUnknown},
		{domain.ExecutionOutcomeFailed, domain.FailureCauseQuota, domain.FailureCauseQuota},
		{domain.ExecutionOutcomeCanceled, "", ""},
		{domain.ExecutionOutcomeLost, "", ""},
		{domain.ExecutionOutcomeBlocked, "", ""},
	} {
		outcome := domain.ExecutionOutcome{Status: tc.status, Cause: tc.cause}
		outcome.CanonicalizeStoredRow()
		if outcome.Cause != tc.want {
			t.Errorf("%s with cause %q canonicalized to %q, want %q", tc.status, tc.cause, outcome.Cause, tc.want)
		}
	}
}

// An execution-failure item stored before its facts carried a cause reads
// back with the cause its outcome reconstructs with (#2210).
func TestAttentionItemCanonicalizeStoredRowExecutionFailureCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status domain.ExecutionOutcomeStatus
		cause  domain.FailureCause
		want   domain.FailureCause
	}{
		{domain.ExecutionOutcomeFailed, "", domain.FailureCauseUnknown},
		{domain.ExecutionOutcomeFailed, domain.FailureCauseQuota, domain.FailureCauseQuota},
		{domain.ExecutionOutcomeCanceled, "", ""},
		{domain.ExecutionOutcomeLost, "", ""},
	} {
		item := domain.AttentionItem{ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: tc.status, Cause: tc.cause,
		}}
		item.CanonicalizeStoredRow()
		if item.ExecutionFailure.Cause != tc.want {
			t.Errorf("%s item with cause %q canonicalized to %q, want %q",
				tc.status, tc.cause, item.ExecutionFailure.Cause, tc.want)
		}
	}
	var item domain.AttentionItem
	item.CanonicalizeStoredRow()
	if item.ExecutionFailure != nil {
		t.Errorf("item without facts canonicalized to %+v", item.ExecutionFailure)
	}
}
