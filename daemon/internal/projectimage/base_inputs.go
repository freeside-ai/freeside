package projectimage

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// ObserveBaseInputs reads, from one exact commit in checkoutDir, what that
// commit holds for every input a project image's environment records. It is
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
) (domain.ProjectImageBaseInputs, error) {
	reader, err := verify.OpenCommitReader(ctx, gitPath, checkoutDir)
	if err != nil {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe %s at run base: %w", unsupportedInputNames[0], err)
	}
	defer reader.Close()
	return observeBaseInputs(ctx, reader, commitSHA)
}

func observeBaseInputs(ctx context.Context, reader *verify.CommitReader, commitSHA string) (domain.ProjectImageBaseInputs, error) {
	inputs := domain.ProjectImageBaseInputs{CommitSHA: commitSHA}
	for _, name := range unsupportedInputNames {
		// Only existence matters, in any shape: the builder refuses a commit
		// that has an entry here at all, and the helper refuses a workspace
		// that does. The one-byte cap reads nothing worth keeping, and an
		// entry the reader refuses still exists.
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
	for name, hash := range map[string]*string{
		"package.json":      &inputs.PackageJSONSHA256,
		"package-lock.json": &inputs.PackageLockSHA256,
	} {
		content, present, err := reader.ReadFile(ctx, commitSHA, name, maxManifestBytes)
		if err != nil {
			return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe %s at run base: %w", name, err)
		}
		if present {
			*hash = manifestSHA256(content)
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
