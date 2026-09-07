package publish

import "testing"

func TestCandidateBodyBudgetReservesEveryPublisherOwnedSection(t *testing.T) {
	t.Parallel()
	want := maxPullRequestBodyBytes - 4*len("\n\n") - identityMarkerBytes -
		maxRenderedAdvisoriesBytes - maxRenderedScopeDecisionBytes - minRenderedDispositionHistoryBytes
	if maxCandidateBodyBytes != want {
		t.Fatalf("candidate body budget = %d, want derived %d", maxCandidateBodyBytes, want)
	}
}
