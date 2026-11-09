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
// commit holds for every input a project image of layout records, for each
// toolchain the layout includes: the root npm manifests and npm's
// unsupported inputs, the Go module's go.mod and go.sum at its directory and
// Go's unsupported inputs, or the Swift package's Package.swift and
// Package.resolved at its directory and Swift's unsupported inputs. It is
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
	type manifest struct {
		name string
		hash *string
	}
	var (
		unsupported []string
		// manifests is ordered, toolchain by toolchain in Members order, so
		// the unsupported inputs a manifest reports come out in a fixed order.
		manifests []manifest
		goMod     string
		resolved  string
	)
	for _, member := range layout.Toolchain.Members() {
		switch member {
		case domain.ProjectToolchainNPM:
			unsupported = append(unsupported, unsupportedInputNames...)
			manifests = append(manifests,
				manifest{"package.json", &inputs.PackageJSONSHA256},
				manifest{"package-lock.json", &inputs.PackageLockSHA256})
		case domain.ProjectToolchainGo:
			unsupported = append(unsupported, goUnsupportedEntries(layout.GoModuleDir)...)
			goMod = path.Join(layout.GoModuleDir, "go.mod")
			manifests = append(manifests,
				manifest{goMod, &inputs.GoModSHA256},
				manifest{path.Join(layout.GoModuleDir, "go.sum"), &inputs.GoSumSHA256})
		case domain.ProjectToolchainSwift:
			unsupported = append(unsupported, swiftUnsupportedEntries(layout.SwiftPackageDir)...)
			resolved = path.Join(layout.SwiftPackageDir, "Package.resolved")
			manifests = append(manifests,
				manifest{path.Join(layout.SwiftPackageDir, "Package.swift"), &inputs.PackageSwiftSHA256},
				manifest{resolved, &inputs.PackageResolvedSHA256})
		case domain.ProjectToolchainGoSwift:
			// Members never yields a composite.
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
	for _, m := range manifests {
		name := m.name
		content, present, err := reader.ReadFile(ctx, commitSHA, name, maxManifestBytes)
		if err != nil {
			return domain.ProjectImageBaseInputs{}, fmt.Errorf("observe %s at run base: %w", name, err)
		}
		if !present {
			continue
		}
		*m.hash = manifestSHA256(content)
		if name == goMod && goModHasLocalReplace(content) {
			inputs.UnsupportedInputs = append(inputs.UnsupportedInputs,
				goLocalReplaceInput(layout.GoModuleDir))
		}
		if name == resolved && checkSwiftPackageResolved(content) != nil {
			inputs.UnsupportedInputs = append(inputs.UnsupportedInputs,
				swiftUnsupportedResolvedInput(layout.SwiftPackageDir))
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
