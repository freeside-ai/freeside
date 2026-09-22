package publish

import "testing"

func TestCandidateBodyBudgetReservesEveryPublisherOwnedSection(t *testing.T) {
	t.Parallel()
	want := maxPullRequestBodyBytes - 6*len("\n\n") - identityMarkerBytes -
		maxRenderedVerificationBytes - maxRenderedAdvisoriesBytes - maxRenderedScopeDecisionBytes -
		maxRenderedSourceReferenceBytes - minRenderedDispositionHistoryBytes
	if maxCandidateBodyBytes != want {
		t.Fatalf("candidate body budget = %d, want derived %d", maxCandidateBodyBytes, want)
	}
}
