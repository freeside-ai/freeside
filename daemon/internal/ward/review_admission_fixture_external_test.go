package ward_test

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// testReviewAdmission is the admission snapshot a fixture review request
// carries where the test is not about admission. A review source launches
// the identity, model, and effort the snapshot names, so it names the ones
// the Codex fixture source runs (testCodexReview).
func testReviewAdmission(runID domain.RunID) domain.ReviewAdmission {
	admission := fake.ReviewAdmission(domain.RoleReviewer, runID, "review-fixture")
	admission.AuthIdentityID = "codex-reviewer"
	admission.AgentBinding.RouteModelID, admission.AgentBinding.NativeEffort = "gpt-5.2-codex", "high"
	return admission
}
