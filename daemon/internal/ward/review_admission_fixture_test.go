package ward

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// testReviewAdmission is the admission snapshot a fixture review request
// carries where the test is not about admission. A review source launches
// the identity, model, and effort the snapshot names, so it names the ones
// the Codex fixture source runs (testCodexReview).
func testReviewAdmission(runID domain.RunID) domain.ReviewAdmission {
	admission := testRecordedReviewAdmission(runID)
	admission.AuthIdentityID = "codex-reviewer"
	admission.AgentBinding.RouteModelID, admission.AgentBinding.NativeEffort = "gpt-5.2-codex", "high"
	return admission
}

// testClaudeReviewAdmission is testReviewAdmission for the Claude fixture
// source (testClaudeReview).
func testClaudeReviewAdmission(runID domain.RunID) domain.ReviewAdmission {
	admission := testRecordedReviewAdmission(runID)
	admission.AuthIdentityID = "claude-reviewer"
	admission.AgentBinding.RouteModelID, admission.AgentBinding.NativeEffort = "claude-opus-4-8", "high"
	return admission
}

// testRecordedReviewAdmission is a snapshot no fixture source launches, for a
// test that only reads a recorded review, such as an outcome golden.
func testRecordedReviewAdmission(runID domain.RunID) domain.ReviewAdmission {
	return fake.ReviewAdmission(domain.RoleReviewer, runID, "review-fixture")
}

// testSourceReviewAdmission is testReviewAdmission for whichever fixture
// source sourceConfig composes, for a test shared by both providers.
func testSourceReviewAdmission(runID domain.RunID, sourceConfig CodexReviewSourceConfig) domain.ReviewAdmission {
	admission := testRecordedReviewAdmission(runID)
	admission.AuthIdentityID = sourceConfig.AuthIdentityID
	admission.AgentBinding.RouteModelID = sourceConfig.Review.Model
	admission.AgentBinding.NativeEffort = sourceConfig.Review.ReasoningEffort
	return admission
}
