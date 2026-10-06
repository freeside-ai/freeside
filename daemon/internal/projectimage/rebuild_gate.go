package projectimage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// RebuildDecision is the policy-gated rebuild's verdict for one candidate
// (plan §5.7). Exactly one of three states holds: the gate does not apply
// (Needed is false), it refuses (Refusal names the clause), or it holds (Needed
// with an empty Refusal) and the image may be rebuilt at the head.
type RebuildDecision struct {
	// Needed is false when the head's dependency manifests are the ones the
	// image baked, or the image records no environment to compare with. The
	// image then verifies the head as it always did, and nothing else in the
	// decision is set but Head.
	Needed bool
	// Refusal is the clause that failed, empty when the gate holds.
	Refusal RebuildRefusal
	// Detail says what failed the clause. It may quote text the candidate
	// wrote, always bounded and quoted.
	Detail string
	// Head is what the head commit holds for the inputs an image records.
	Head domain.ProjectImageBaseInputs
	// Delta summarizes the lockfile change of a decision that holds.
	Delta LockfileDelta
}

// EvaluateRebuild decides whether a candidate that changes its dependency
// manifests stays inside the project's declared policy, so the image may be
// rebuilt at headSHA without a person.
//
// The gate applies only to a head whose package.json or package-lock.json
// differs from the ones image baked. It then holds when all of these do:
//
//   - The head holds neither npm-shrinkwrap.json nor .npmrc.
//   - The head declares the verification recipe its base declares.
//   - The run's policy declares a valid registry set.
//   - The head's manifests change nothing but dependencies the registry
//     serves, pinned in a consistent npm v2 or v3 lockfile to https URLs on
//     declared hosts (evaluateLockfileDelta has the exact rules).
//
// The manifests are compared with the base's, not with the image's, because
// only hashes of the image's manifests are recorded. The caller has already
// proved the image serves baseSHA, which makes the two the same bytes.
//
// checkoutDir must hold both commits. The caller owns proving it is the
// repository image names. A refusal is a decision, never an error; an error
// is a fault reading the checkout.
func EvaluateRebuild(
	ctx context.Context,
	gitPath string,
	checkoutDir string,
	image domain.ProjectImage,
	baseSHA string,
	headSHA string,
	policy domain.ResolvedPolicy,
) (RebuildDecision, error) {
	refuse := func(head domain.ProjectImageBaseInputs, clause RebuildRefusal, format string, args ...any) RebuildDecision {
		return RebuildDecision{
			Needed: true, Refusal: clause, Detail: fmt.Sprintf(format, args...), Head: head,
		}
	}
	head, err := ObserveBaseInputs(ctx, gitPath, checkoutDir, headSHA)
	if errors.Is(err, verify.ErrCommitFileUnreadable) {
		if image.Environment == nil {
			return RebuildDecision{Head: domain.ProjectImageBaseInputs{CommitSHA: headSHA}}, nil
		}
		// A manifest or recipe in a shape no build can read (a symlink, an
		// oversized blob). Whether the manifests changed cannot be told, and
		// no image could be built from them, so this is the gate's refusal.
		return refuse(domain.ProjectImageBaseInputs{CommitSHA: headSHA},
			RebuildRefusalLockfileInconsistent,
			"a dependency manifest or the recipe at the candidate is not a readable regular file"), nil
	}
	if err != nil {
		return RebuildDecision{}, fmt.Errorf("observe project-image inputs at head %s: %w", headSHA, err)
	}
	if image.Environment == nil ||
		(head.PackageJSONSHA256 == image.Environment.PackageJSONSHA256 &&
			head.PackageLockSHA256 == image.Environment.PackageLockSHA256) {
		return RebuildDecision{Head: head}, nil
	}
	if len(head.UnsupportedInputs) != 0 {
		return refuse(head, RebuildRefusalUnsupportedInput,
			"the candidate contains %s", strings.Join(head.UnsupportedInputs, ", ")), nil
	}
	base, err := ObserveBaseInputs(ctx, gitPath, checkoutDir, baseSHA)
	if err != nil {
		return RebuildDecision{}, fmt.Errorf("observe project-image inputs at base %s: %w", baseSHA, err)
	}
	if head.RecipeDigest != base.RecipeDigest {
		return refuse(head, RebuildRefusalRecipeChanged,
			"the candidate declares verification recipe %s and its base declares %s",
			strconv.Quote(string(head.RecipeDigest)), strconv.Quote(string(base.RecipeDigest))), nil
	}
	registries, declared, err := domain.RegistrySetFromPolicy(policy)
	if err != nil {
		return refuse(head, RebuildRefusalNoRegistrySet,
			"the run's policy declares a malformed %s", domain.RegistrySetPolicyKey), nil
	}
	if !declared {
		return refuse(head, RebuildRefusalNoRegistrySet,
			"the run's policy declares no %s", domain.RegistrySetPolicyKey), nil
	}
	read := func(commitSHA, name string) ([]byte, error) {
		content, present, err := verify.ReadFileAtCommit(
			ctx, gitPath, checkoutDir, commitSHA, name, maxManifestBytes)
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", name, commitSHA, err)
		}
		if !present {
			// Not an error: an absent manifest parses as no JSON object and
			// the lockfile reader refuses it under its own clause.
			return nil, nil
		}
		return content, nil
	}
	basePackageJSON, err := read(baseSHA, "package.json")
	if err != nil {
		return RebuildDecision{}, err
	}
	basePackageLock, err := read(baseSHA, "package-lock.json")
	if err != nil {
		return RebuildDecision{}, err
	}
	headPackageJSON, err := read(headSHA, "package.json")
	if err != nil {
		return RebuildDecision{}, err
	}
	headPackageLock, err := read(headSHA, "package-lock.json")
	if err != nil {
		return RebuildDecision{}, err
	}
	delta, refusal := evaluateLockfileDelta(
		basePackageJSON, basePackageLock, headPackageJSON, headPackageLock, registries)
	if refusal != nil {
		return refuse(head, refusal.clause, "%s", refusal.detail), nil
	}
	return RebuildDecision{Needed: true, Head: head, Delta: delta}, nil
}

// BuildInputs are the operator-configured, host-specific inputs a rebuild
// needs and no image record carries: how this host reaches the approved base
// image and the network.
type BuildInputs struct {
	// BaseBuildRef is the local reference the container runtime resolves to
	// the approved base image (Request.BaseBuildRef).
	BaseBuildRef string
	// BuildProxy and DNS are the build's egress, as at onboarding.
	BuildProxy string
	DNS        []string
}

// Validate applies the builder's own shape rules to the operator's inputs, so
// a daemon refuses a malformed one at startup instead of at every rebuild.
func (i BuildInputs) Validate() error {
	if err := validateBaseBuildRef(i.BaseBuildRef); err != nil {
		return err
	}
	if err := validateBuildDNS(i.DNS); err != nil {
		return err
	}
	return ValidateBuildProxy(i.BuildProxy)
}

// RebuildRequest derives the request that rebuilds image at headSHA from the
// local checkout sourceDir. Everything the image record binds carries over
// unchanged: the repository, the recipe (recipe must hash to the recorded
// digest), the base image, and the registry destination, which is parsed back
// out of the image's own reference. Only the commit, the tag, and the source
// differ.
//
// The tag names the head, so two candidates never contend for one tag; the
// run binds to the digest the build publishes, never to the tag.
func RebuildRequest(
	image domain.ProjectImage,
	headSHA string,
	sourceDir string,
	recipe []byte,
	inputs BuildInputs,
) (Request, error) {
	if !commitPattern.MatchString(headSHA) {
		return Request{}, fmt.Errorf("rebuild commit %q is not a full lowercase SHA-1: %w",
			headSHA, ErrInvalidRequest)
	}
	if sourceDir == "" {
		return Request{}, fmt.Errorf("rebuild source checkout is required: %w", ErrInvalidRequest)
	}
	if got := verify.RecipeDigest(recipe); got != image.RecipeDigest {
		return Request{}, fmt.Errorf("rebuild recipe digest %s, want the image's %s: %w",
			got, image.RecipeDigest, ErrInvalidRequest)
	}
	destination, _, _ := strings.Cut(string(image.ImageRef), "@")
	separator := strings.LastIndex(destination, "/")
	if separator <= 0 {
		return Request{}, fmt.Errorf("project image reference %q names no registry: %w",
			image.ImageRef, ErrInvalidRequest)
	}
	request := Request{
		Repository:   image.Repository,
		RepositoryID: image.RepositoryID,
		CommitSHA:    headSHA,
		SourceDir:    sourceDir,
		Recipe:       recipe,
		BaseImageRef: image.BaseImageRef,
		BaseBuildRef: inputs.BaseBuildRef,
		ImageName:    destination[separator+1:],
		RefTag:       "rebuild-" + headSHA[:12],
		DNS:          append([]string{}, inputs.DNS...),
		BuildProxy:   inputs.BuildProxy,
	}
	registry := destination[:separator]
	if port, local := strings.CutPrefix(registry, "127.0.0.1:"); local {
		request.LocalRegistryPort, _ = strconv.Atoi(port)
	} else {
		request.Registry = registry
	}
	// The parse above is a convenience; this is the proof that the derived
	// destination is exactly the admitted image's.
	if err := ValidatePublishedRef(request, image.ImageRef); err != nil {
		return Request{}, fmt.Errorf("derive rebuild destination from %q: %w: %w",
			image.ImageRef, err, ErrInvalidRequest)
	}
	return request, nil
}
