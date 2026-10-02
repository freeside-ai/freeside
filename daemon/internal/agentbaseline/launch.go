package agentbaseline

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Output contract identifiers. The launch pins which contract applies; the
// stage enforces it (the fixed-path outcome files for the writer stages, the
// review result schema for review).
const (
	SpecificationOutputContract  = "freeside.specification.outcome/v1"
	ImplementationOutputContract = "freeside.implementation.outcome/v1"
	ReviewOutputContract         = "freeside.review.result/v1"
)

// Launch returns the launch a ward stage hands its harness. The values state
// what the daemon's launches do today: every launch is one-shot and severed
// from user-level configuration, the two writer stages run a writable
// workspace and only observe auxiliary inference, and review is read-only
// and forbids it (§5.4, "The Stage Owns the Launch"). A stage with no ward
// launch (verification) has none.
func Launch(stage domain.StageName) (domain.LaunchSpec, error) {
	launch := domain.LaunchSpec{
		EncodingVersion: domain.LaunchEncodingVersion, Stage: stage,
		Severance: true, SessionMode: domain.SessionOneShot,
	}
	switch stage {
	case domain.StageNameSpecification:
		launch.Writer, launch.OutputContract = true, SpecificationOutputContract
		launch.AuxiliaryInference = domain.AuxiliaryObserved
	case domain.StageNameImplementation:
		launch.Writer, launch.OutputContract = true, ImplementationOutputContract
		launch.AuxiliaryInference = domain.AuxiliaryObserved
	case domain.StageNameReview:
		launch.OutputContract = ReviewOutputContract
		launch.AuxiliaryInference = domain.AuxiliaryForbidden
	case domain.StageNameVerification:
		// Verification is an engine job in a clean ward: no harness, no agent.
		return domain.LaunchSpec{}, fmt.Errorf("stage %q has no ward launch: %w", stage, domain.ErrInvalidStageName)
	}
	if launch.OutputContract == "" {
		return domain.LaunchSpec{}, fmt.Errorf("stage %q: %w", stage, domain.ErrInvalidStageName)
	}
	digest, err := launch.ComputeDigest()
	if err != nil {
		return domain.LaunchSpec{}, err
	}
	launch.Digest = digest
	return launch, nil
}

// RoleLaunch returns the launch of the stage a ward role runs in.
func RoleLaunch(role domain.RoleName) (domain.LaunchSpec, error) {
	stage, ok := role.Stage()
	if !ok {
		return domain.LaunchSpec{}, fmt.Errorf("role %q runs in no ward stage: %w", role, domain.ErrInvalidStageName)
	}
	return Launch(stage)
}
