package projectimage

import (
	"context"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// UpkeepAdoption is what an image-policy adoption card would adopt to answer
// a policy-class upkeep refusal at a tip (plan §5.7, Project-image upkeep):
// the tip's recipe digest when the recipe changed, and the hosts the tip's
// changed lockfile entries resolve from that the repository's effective
// registry set does not hold. Hosts is sorted and never nil.
type UpkeepAdoption struct {
	RecipeDigest *domain.Digest
	Hosts        []string
}

// DeriveUpkeepAdoption re-derives an adoption card's facts from the tree at
// tipSHA, measured from image's build commit as EvaluateUpkeep measures it.
// The refusal record keeps clause names and free-text detail only, so this
// is the card's one source for the exact digest and hosts.
//
// adoptable is false when no adoption could answer the delta: the
// effective registry set is malformed (the operator's declaration is broken,
// and an adopted host cannot repair it), or the lockfile delta refuses on a
// clause other than the authority check, which makes the refusal a repair
// rather than a policy refusal. checkoutDir must hold the build commit and
// tipSHA.
func DeriveUpkeepAdoption(
	ctx context.Context,
	gitPath string,
	checkoutDir string,
	image domain.ProjectImage,
	tipSHA string,
	policy domain.ResolvedPolicy,
	adoptedHosts []string,
) (adoption UpkeepAdoption, adoptable bool, err error) {
	reader, err := verify.OpenCommitReader(ctx, gitPath, checkoutDir)
	if err != nil {
		return UpkeepAdoption{}, false, fmt.Errorf("observe project-image inputs at tip %s: %w", tipSHA, err)
	}
	defer reader.Close()
	measured, err := measuredForUpkeep(ctx, reader, image)
	if err != nil {
		return UpkeepAdoption{}, false, err
	}
	layout := image.InputLayout()
	head, err := observeBaseInputs(ctx, reader, tipSHA, layout)
	if err != nil {
		return UpkeepAdoption{}, false, fmt.Errorf("observe project-image inputs at tip %s: %w", tipSHA, err)
	}
	base, err := observeBaseInputs(ctx, reader, image.CommitSHA, layout)
	if err != nil {
		return UpkeepAdoption{}, false, fmt.Errorf(
			"observe project-image inputs at build commit %s: %w", image.CommitSHA, err)
	}
	adoption.Hosts = []string{}
	if head.RecipeDigest != base.RecipeDigest {
		digest := head.RecipeDigest
		adoption.RecipeDigest = &digest
	}
	if len(measured.Environment.ChangedManifests(head)) == 0 {
		return adoption, true, nil
	}
	registries, declared, err := domain.EffectiveRegistrySet(policy, adoptedHosts)
	if err != nil {
		return UpkeepAdoption{}, false, nil
	}
	// With no registry set the authority check is skipped, so every finding
	// left is a clause no adoption answers, and the delta's hosts are every
	// host the changed dependencies resolve from.
	delta, answerable, err := hostDelta(ctx, reader, layout, head, image.CommitSHA, tipSHA)
	if err != nil || !answerable {
		return UpkeepAdoption{}, false, err
	}
	for _, host := range delta.Hosts {
		if !declared || !slices.Contains(registries.Hosts, host) {
			adoption.Hosts = append(adoption.Hosts, host)
		}
	}
	return adoption, true, nil
}

// hostDelta measures the dependency delta from baseSHA to tipSHA with the
// authority check skipped. answerable is false when any other clause refuses.
func hostDelta(
	ctx context.Context, reader *verify.CommitReader, layout domain.ProjectImageInputLayout,
	head domain.ProjectImageBaseInputs, baseSHA, tipSHA string,
) (delta LockfileDelta, answerable bool, err error) {
	var findings gateFindings
	switch layout.Toolchain {
	case domain.ProjectToolchainNPM:
		delta, findings, err = readLockfileDelta(ctx, reader, baseSHA, tipSHA, nil)
		if err != nil {
			return LockfileDelta{}, false, err
		}
		return delta, len(findings) == 0, nil
	case domain.ProjectToolchainGo:
		if len(head.UnsupportedInputs) != 0 {
			return LockfileDelta{}, false, nil
		}
		read := func(commitSHA, name string) ([]byte, error) {
			return readManifest(ctx, reader, commitSHA, name)
		}
		base, tip, refusal, err := readGoModuleChange(read, layout.GoModuleDir, head, baseSHA)
		if err != nil || refusal != nil {
			return LockfileDelta{}, false, err
		}
		delta, findings = evaluateGoModuleDelta(base, tip, nil)
		return delta, len(findings) == 0, nil
	}
	return LockfileDelta{}, false, fmt.Errorf("project image toolchain %q: %w", layout.Toolchain, domain.ErrProjectImageInvalid)
}
