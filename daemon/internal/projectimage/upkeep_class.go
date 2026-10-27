package projectimage

import (
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// upkeepClassPrecedence orders the refusal classes for findings that span
// several: a class that opens no task wins, so the job never proposes or
// repairs what a delta it cannot bound would refuse again. A host fault
// opens no task either, though no gate verdict mixes one with a clause.
var upkeepClassPrecedence = []domain.ImageUpkeepRefusalClass{
	domain.ImageUpkeepRefusalGoModuleDelta,
	domain.ImageUpkeepRefusalUnsupportedNPMInput,
	domain.ImageUpkeepRefusalHostFault,
	domain.ImageUpkeepRefusalRepair,
	domain.ImageUpkeepRefusalPolicy,
}

// ClassifyUpkeepFindings sorts an upkeep refusal by what may answer it (plan
// §5.7, Project-image upkeep), from every clause that refused. findings must
// name at least one clause; a build that failed with none is classified by
// ClassifyUpkeepBuildFailure.
func ClassifyUpkeepFindings(findings []domain.ImageUpkeepClauseFinding) (domain.ImageUpkeepRefusalClass, error) {
	if len(findings) == 0 {
		return "", fmt.Errorf("classify upkeep refusal: no clause refused: %w", domain.ErrImageUpkeepRefusalInvalid)
	}
	rank := len(upkeepClassPrecedence)
	for _, finding := range findings {
		index := slices.Index(upkeepClassPrecedence, upkeepClauseClass(finding.Clause))
		if index < 0 {
			return "", fmt.Errorf("classify upkeep refusal clause %q: %w",
				finding.Clause, domain.ErrImageUpkeepRefusalInvalid)
		}
		rank = min(rank, index)
	}
	return upkeepClassPrecedence[rank], nil
}

func upkeepClauseClass(clause RebuildRefusal) domain.ImageUpkeepRefusalClass {
	switch clause {
	case RebuildRefusalGoDependencyChanged:
		return domain.ImageUpkeepRefusalGoModuleDelta
	case RebuildRefusalUnsupportedInput:
		return domain.ImageUpkeepRefusalUnsupportedNPMInput
	case RebuildRefusalUnpinnedSource, RebuildRefusalLockfileInconsistent, RebuildRefusalProofFailed:
		return domain.ImageUpkeepRefusalRepair
	case RebuildRefusalRecipeChanged, RebuildRefusalUndeclaredAuthority, RebuildRefusalNoRegistrySet:
		return domain.ImageUpkeepRefusalPolicy
	case RebuildRefusalNotConfigured:
		return domain.ImageUpkeepRefusalHostFault
	}
	return ""
}

// ClassifyUpkeepBuildFailure sorts a rebuild the gate admitted and the build
// then failed. Only the builder's positive evidence against the tree is a
// repair: a proof that ran and failed, or a deterministic install failure.
// Anything else (the registry, the proxy, the container runtime, a drifted
// base tag) is a host fault, as is a build the time bound ended, which the
// caller reports and this does not see. So is an invalid request: every
// ErrInvalidRequest names the image record or the host's build inputs,
// never the tip's tree, and a repair would send a maintenance task to fix a
// repository that did not cause it. The publication lane still refuses the
// one run for it, because there the refusal holds only that run.
func ClassifyUpkeepBuildFailure(err error) domain.ImageUpkeepRefusalClass {
	if errors.Is(err, ErrProofFailed) || errors.Is(err, ErrInstallFailed) {
		return domain.ImageUpkeepRefusalRepair
	}
	return domain.ImageUpkeepRefusalHostFault
}
