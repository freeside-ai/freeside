package publish

import "testing"

func TestCandidateBodyBudgetReservesEveryPublisherOwnedSection(t *testing.T) {
	t.Parallel()
	want := maxPullRequestBodyBytes - 5*len("\n\n") - identityMarkerBytes -
		maxRenderedVerificationBytes - maxRenderedAdvisoriesBytes - maxRenderedScopeDecisionBytes - minRenderedDispositionHistoryBytes
	if maxCandidateBodyBytes != want {
		t.Fatalf("candidate body budget = %d, want derived %d", maxCandidateBodyBytes, want)
	}
}
