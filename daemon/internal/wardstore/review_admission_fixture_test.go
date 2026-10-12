package wardstore

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// testReviewAdmission is the admission snapshot a fixture review request
// carries where the test is not about admission.
func testReviewAdmission(runID domain.RunID) domain.ReviewAdmission {
	return fake.ReviewAdmission(domain.RoleReviewer, runID, "review-fixture")
}
