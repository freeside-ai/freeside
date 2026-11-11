package publish

import "testing"

func TestCandidateBodyBudgetReservesEveryPublisherOwnedSection(t *testing.T) {
	t.Parallel()
	want := maxPullRequestBodyBytes - 6*len("\n\n") - identityMarkerBytes -
		maxRenderedVerificationSectionBytes - maxRenderedAdvisoriesBytes - maxRenderedScopeDecisionBytes -
		maxRenderedSourceReferenceBytes
	if maxCandidateBodyBytes != want {
		t.Fatalf("candidate body budget = %d, want derived %d", maxCandidateBodyBytes, want)
	}
	// The Verification section took over the disposition history's reserved
	// floor (#3022) without shrinking the prose budget, so prose stored and
	// accepted before that change still fits.
	if maxCandidateBodyBytes != 22406 {
		t.Fatalf("candidate body budget = %d, want the unchanged 22406", maxCandidateBodyBytes)
	}
}
