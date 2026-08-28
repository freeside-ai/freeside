package publish

import "testing"

func TestCandidateBodyBudgetReservesEveryPublisherOwnedSection(t *testing.T) {
	t.Parallel()
	want := maxPullRequestBodyBytes - 3*len("\n\n") - identityMarkerBytes -
		maxRenderedAdvisoriesBytes - minRenderedDispositionHistoryBytes
	if maxCandidateBodyBytes != want {
		t.Fatalf("candidate body budget = %d, want derived %d", maxCandidateBodyBytes, want)
	}
}
