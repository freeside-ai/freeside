package projectimage

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// ObserveBaseInputs reads, from one exact commit in checkoutDir, what that
// commit holds for every input a project image of layout records: the root
// npm manifests and npm's unsupported inputs, or the Go module's go.mod and
// go.sum at its directory and Go's unsupported inputs. It is
// the base half of domain.ProjectImage.AdmissibleAt: the image record supplies
// what was baked, this supplies what a run at commitSHA would bring.
//
// It reads the commit's tree through the verifier's hardened plumbing, so the
// checkout needs the commit's objects but no worktree. The caller owns proving
// that checkoutDir is the intended repository and holds the intended commit.
//
// A manifest the base does not hold comes back as an empty hash, which no
// record matches. A recipe it does not hold comes back as an empty digest,
// which AdmissibleAt reads as "declares none": a repository onboarded with a
// recipe supplied outside its tree never holds one. An entry the base holds in
// a shape the builder would refuse (a symlink, an oversized blob) is an error:
// there are no bytes to compare.
func ObserveBaseInputs(
	ctx context.Context,
	gitPath string,
	checkoutDir string,
	commitSHA string,
	layout domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	if err := layout.Validate(); err != nil {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe run base: %w", err)
	}
	reader, err := verify.OpenCommitReader(ctx, gitPath, checkoutDir)
	if err != nil {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe run base inputs: %w", err)
	}
	defer reader.Close()
	return observeBaseInputs(ctx, reader, commitSHA, layout)
}

func observeBaseInputs(
	ctx context.Context,
	reader *verify.CommitReader,
	commitSHA string,
	layout domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	inputs := domain.ProjectImageBaseInputs{CommitSHA: commitSHA}
	var (
		unsupported []string
		manifests   map[string]*string
		goMod       string
	)
	switch layout.Toolchain {
	case domain.ProjectToolchainNPM:
		unsupported = unsupportedInputNames
		manifests = map[string]*string{
			"package.json":      &inputs.PackageJSONSHA256,
			"package-lock.json": &inputs.PackageLockSHA256,
		}
	case domain.ProjectToolchainGo:
		unsupported = goUnsupportedEntries(layout.GoModuleDir)
		goMod = path.Join(layout.GoModuleDir, "go.mod")
		manifests = map[string]*string{
			goMod:                                   &inputs.GoModSHA256,
			path.Join(layout.GoModuleDir, "go.sum"): &inputs.GoSumSHA256,
		}
	}
	for _, name := range unsupported {
		// Only existence matters, in any shape: the builder refuses a commit
		// that has an entry here at all, and the helper refuses a workspace
		// that does. The one-byte cap reads nothing worth keeping, and an
		// entry the reader refuses (a directory such as vendor/ included)
		// still exists.
		_, present, err := reader.ReadFile(ctx, commitSHA, name, 1)
		if errors.Is(err, verify.ErrCommitFileUnreadable) {
			present = true
		} else if err != nil {
			return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe %s at run base: %w", name, err)
		}
		if present {
			inputs.UnsupportedInputs = append(inputs.UnsupportedInputs, name)
		}
	}
	for name, hash := range manifests {
		content, present, err := reader.ReadFile(ctx, commitSHA, name, maxManifestBytes)
		if err != nil {
			return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe %s at run base: %w", name, err)
		}
		if !present {
			continue
		}
		*hash = manifestSHA256(content)
		if name == goMod && goModHasLocalReplace(content) {
			inputs.UnsupportedInputs = append(inputs.UnsupportedInputs,
				goLocalReplaceInput(layout.GoModuleDir))
		}
	}
	recipe, present, err := reader.ReadFile(
		ctx, commitSHA, verify.DefaultRecipePath, verify.DefaultMaxRecipeBytes)
	if err != nil {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe recipe at run base: %w", err)
	}
	if present {
		inputs.RecipeDigest = verify.RecipeDigest(recipe)
	}
	return inputs, nil
}
