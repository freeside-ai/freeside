package fake

import (
	"context"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ReviewAdmissionAt is the admission instant every fixture snapshot carries.
var ReviewAdmissionAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// ReviewAdmission is a fixed, valid admission snapshot for one review
// invocation: the fixture a request, a result, or a record carries where the
// test is not about admission. Two calls with the same arguments are equal,
// so a source that returns the request's snapshot matches what the engine
// stored. The shadow arm names another agent and lineage group, so a test
// that crosses the arms shows it.
func ReviewAdmission(
	role domain.RoleName, runID domain.RunID, id domain.InvocationID,
) domain.ReviewAdmission {
	digest := func(fill string) domain.Digest {
		addr, _ := contentaddr.FromHex(strings.Repeat(fill, 64))
		return domain.Digest(addr)
	}
	admission := domain.ReviewAdmission{
		InvocationID: id, RunID: runID, Role: role,
		AuthIdentityID: "fake-review-identity", LineageGroup: "fake",
		AgentBinding: domain.AdmissionAgentBinding{
			AgentDigest: digest("a"), LaunchDigest: digest("b"), TreatmentDigest: digest("d"),
			PricingRevision: "fake-pricing", LineupRevision: digest("c"),
			EnrollmentID: "fake-review-enrollment", EnrollmentGeneration: 1,
			StoreManifestDigest: digest("9"),
			EffectiveEgress:     []string{"review.fake.example:443"},
			RouteModelID:        "fake", RequestedEffort: domain.EffortHigh, NativeEffort: "review-v1",
			SelectionSource: domain.AgentSelectionSourceLineup,
		},
		AdmittedAt: ReviewAdmissionAt,
	}
	if role == domain.RoleShadowReviewer {
		admission.AuthIdentityID, admission.LineageGroup = "fake-shadow-identity", "fake-shadow"
		admission.AgentBinding.AgentDigest = digest("e")
		admission.AgentBinding.EnrollmentID = "fake-shadow-enrollment"
	}
	return admission
}

// ReviewIdentities are the identities the ReviewAdmission fixture names. A
// review admission takes a slot on its identity's usage pool, so a store under
// a workflow composed over the fake must record them.
func ReviewIdentities() []domain.AuthIdentity {
	return []domain.AuthIdentity{
		{ID: "fake-review-identity", Provider: "fake"},
		{ID: "fake-shadow-identity", Provider: "fake"},
	}
}

// RecordAdmission is the ReviewAdmission fixture as a review record carries
// it.
func RecordAdmission(
	role domain.RoleName, runID domain.RunID, id domain.InvocationID,
) *domain.ReviewAdmission {
	admission := ReviewAdmission(role, runID, id)
	return &admission
}

// SeededReviewAdmission is RecordAdmission under the lineage group, model,
// and effort a seeded store shows for its reviewer, so a seeded round reads
// like a real one.
func SeededReviewAdmission(
	runID domain.RunID, id domain.InvocationID,
) *domain.ReviewAdmission {
	admission := ReviewAdmission(domain.RoleReviewer, runID, id)
	admission.LineageGroup = "openai"
	admission.AgentBinding.RouteModelID, admission.AgentBinding.NativeEffort = "gpt-codex", "high"
	return &admission
}

// AdmitReview admits every review invocation with the ReviewAdmission
// fixture. It has the shape of the engine's review admitter, for a workflow
// composed over the fake review source.
func AdmitReview(
	_ context.Context, role domain.RoleName, runID domain.RunID,
	id domain.InvocationID, _ time.Time,
) (domain.ReviewAdmission, error) {
	return ReviewAdmission(role, runID, id), nil
}
