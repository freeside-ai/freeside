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
	// Refusal is the clause that failed, empty when the gate holds. When
	// several clauses fail it is the one the gate reaches first, the order
	// the publication lane reports.
	Refusal RebuildRefusal
	// Detail says what failed the clause. It may quote text the candidate
	// wrote, always bounded and quoted.
	Detail string
	// Findings are every clause that failed, Refusal's first, each with the
	// first detail the gate found for it. The upkeep job classifies them.
	Findings []domain.ImageUpkeepClauseFinding
	// Head is what the head commit holds for the inputs an image records.
	Head domain.ProjectImageBaseInputs
	// Delta summarizes the lockfile change of a decision that holds.
	Delta LockfileDelta
}

// EvaluateRebuild decides whether a candidate that changes its dependency
// manifests stays inside the project's declared policy, so the image may be
// rebuilt at headSHA without a person.
//
// The gate applies only to a head whose dependency manifests differ from the
// ones image baked (ProjectImageEnvironment.ChangedManifests), or, for a Go
// image, to a head holding a Go input no image supports. For an npm image it
// holds when all of these do:
//
//   - The head holds neither npm-shrinkwrap.json nor .npmrc.
//   - The head declares the verification recipe its base declares.
//   - The repository's effective registry set is valid and not empty: the
//     set the run's policy declares plus adoptedHosts, the hosts the
//     repository's approved adoption cards added
//     (domain.EffectiveRegistrySet).
//   - The head's manifests change nothing but dependencies the registry
//     serves, pinned in a consistent npm v2 or v3 lockfile to https URLs on
//     hosts in that set (evaluateLockfileDelta has the exact rules).
//
// For a Go image it holds when all of these do, and reports the first that
// fails in this order:
//
//   - The head holds no vendor directory or go.work, and a go.mod and go.sum
//     the reader accepts: no filesystem replace, no go or toolchain directive
//     the baked Go refuses, and go.sum lines of Go's own shape
//     (readGoModuleChange). The base's must read too, or the change cannot
//     be bounded.
//   - The head declares the verification recipe its base declares.
//   - The repository's effective registry set is valid and not empty, as
//     for npm.
//   - go.sum pins every module go.mod requires, and that set holds the
//     module proxy when go.sum changes any module version or go.mod newly
//     requires one (evaluateGoModuleDelta).
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
	adoptedHosts []string,
) (RebuildDecision, error) {
	reader, err := verify.OpenCommitReader(ctx, gitPath, checkoutDir)
	if err != nil {
		return RebuildDecision{}, fmt.Errorf("observe project-image inputs at head %s: observe %s at run base: %w",
			headSHA, unsupportedInputNames[0], err)
	}
	defer reader.Close()
	return evaluateGate(ctx, reader, image, baseSHA, headSHA, policy, adoptedHosts, false)
}

// EvaluateUpkeep is the gate the upkeep job applies to an image that is not
// current at the default-branch tip (plan §5.7, Project-image upkeep). Its
// base is the image's own build commit, and it differs from EvaluateRebuild
// in two ways:
//
//   - It always applies, so Needed is always true: the caller has found the
//     image not current at tipSHA. A tip whose manifests are the ones the
//     image baked still takes the unsupported-input and recipe clauses, and
//     with neither refusing, the toolchain-only delta (a preparation digest
//     other than currentPreparation) holds with no lockfile delta.
//   - A record with no environment is measured against the inputs observed
//     at its build commit, as if it had baked them.
//
// checkoutDir must hold the build commit and tipSHA.
func EvaluateUpkeep(
	ctx context.Context,
	gitPath string,
	checkoutDir string,
	image domain.ProjectImage,
	tipSHA string,
	policy domain.ResolvedPolicy,
	adoptedHosts []string,
) (RebuildDecision, error) {
	reader, err := verify.OpenCommitReader(ctx, gitPath, checkoutDir)
	if err != nil {
		return RebuildDecision{}, fmt.Errorf("observe project-image inputs at tip %s: %w", tipSHA, err)
	}
	defer reader.Close()
	measured, err := measuredForUpkeep(ctx, reader, image)
	if err != nil {
		return RebuildDecision{}, err
	}
	return evaluateGate(ctx, reader, measured, image.CommitSHA, tipSHA, policy, adoptedHosts, true)
}

// measuredForUpkeep is the image as upkeep measures it: a record with no
// environment takes the manifests observed at its build commit, as if it had
// baked them.
func measuredForUpkeep(
	ctx context.Context, reader *verify.CommitReader, image domain.ProjectImage,
) (domain.ProjectImage, error) {
	if image.Environment != nil {
		return image, nil
	}
	built, err := observeBaseInputs(ctx, reader, image.CommitSHA, image.InputLayout())
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf(
			"observe project-image inputs at build commit %s: %w", image.CommitSHA, err)
	}
	// No preparation digest: the record names none, so it is never the
	// current one.
	image.Environment = &domain.ProjectImageEnvironment{
		PackageJSONSHA256: built.PackageJSONSHA256, PackageLockSHA256: built.PackageLockSHA256,
	}
	return image, nil
}

// evaluateGate is the gate both entries share. upkeep selects EvaluateUpkeep's
// rules; without it the verdict is exactly the publication lane's.
func evaluateGate(
	ctx context.Context,
	reader *verify.CommitReader,
	image domain.ProjectImage,
	baseSHA string,
	headSHA string,
	policy domain.ResolvedPolicy,
	adoptedHosts []string,
	upkeep bool,
) (RebuildDecision, error) {
	var findings gateFindings
	decide := func(head domain.ProjectImageBaseInputs, delta LockfileDelta) RebuildDecision {
		decision := RebuildDecision{Needed: true, Head: head}
		if len(findings) == 0 {
			decision.Delta = delta
			return decision
		}
		decision.Refusal, decision.Detail = findings[0].clause, findings[0].detail
		for _, finding := range findings {
			decision.Findings = append(decision.Findings,
				domain.ImageUpkeepClauseFinding{Clause: finding.clause, Detail: finding.detail})
		}
		return decision
	}
	head, err := observeBaseInputs(ctx, reader, headSHA, image.InputLayout())
	if errors.Is(err, verify.ErrCommitFileUnreadable) {
		if image.Environment == nil {
			return RebuildDecision{Head: domain.ProjectImageBaseInputs{CommitSHA: headSHA}}, nil
		}
		// A manifest or recipe in a shape no build can read (a symlink, an
		// oversized blob). Whether the manifests changed cannot be told, and
		// no image could be built from them, so this is the gate's refusal.
		clause := RebuildRefusalLockfileInconsistent
		if image.InputLayout().Toolchain == domain.ProjectToolchainGo {
			clause = RebuildRefusalGoDependencyChanged
		}
		findings.add(refuseLockfile(clause,
			"a dependency manifest or the recipe at the candidate is not a readable regular file"))
		return decide(domain.ProjectImageBaseInputs{CommitSHA: headSHA}, LockfileDelta{}), nil
	}
	if err != nil {
		return RebuildDecision{}, fmt.Errorf("observe project-image inputs at head %s: %w", headSHA, err)
	}
	if image.Environment == nil {
		return RebuildDecision{Head: head}, nil
	}
	changed := image.Environment.ChangedManifests(head)
	recipeClause := func() error {
		base, err := observeBaseInputs(ctx, reader, baseSHA, image.InputLayout())
		if err != nil {
			return fmt.Errorf("observe project-image inputs at base %s: %w", baseSHA, err)
		}
		if head.RecipeDigest != base.RecipeDigest {
			findings.add(refuseLockfile(RebuildRefusalRecipeChanged,
				"the candidate declares verification recipe %s and its base declares %s",
				strconv.Quote(string(head.RecipeDigest)), strconv.Quote(string(base.RecipeDigest))))
		}
		return nil
	}
	// registryClause returns the repository's effective registry set, or
	// nil after refusing the clause. A malformed declaration still refuses,
	// since an adoption cannot repair the operator's own value.
	registryClause := func() *domain.RegistrySet {
		registries, declared, err := domain.EffectiveRegistrySet(policy, adoptedHosts)
		switch {
		case err != nil:
			findings.add(refuseLockfile(RebuildRefusalNoRegistrySet,
				"the run's policy declares a malformed %s", domain.RegistrySetPolicyKey))
		case !declared:
			findings.add(refuseLockfile(RebuildRefusalNoRegistrySet,
				"the run's policy declares no %s", domain.RegistrySetPolicyKey))
		default:
			return &registries
		}
		return nil
	}
	read := func(commitSHA, name string) ([]byte, error) {
		return readManifest(ctx, reader, commitSHA, name)
	}
	if image.InputLayout().Toolchain == domain.ProjectToolchainGo {
		// A Go head that keeps its manifests but adds a vendor directory or a
		// go.work would otherwise fail only inside the room, at the
		// preparation helper; the gate names it instead.
		if len(changed) == 0 && len(head.UnsupportedInputs) == 0 && !upkeep {
			return RebuildDecision{Head: head}, nil
		}
		if len(head.UnsupportedInputs) != 0 {
			findings.add(refuseLockfile(RebuildRefusalGoDependencyChanged,
				"the candidate contains %s", strings.Join(head.UnsupportedInputs, ", ")))
		}
		var baseModule, headModule goModuleManifests
		readable := false
		if len(changed) != 0 {
			var refusal *lockfileRefusal
			baseModule, headModule, refusal, err = readGoModuleChange(
				read, image.InputLayout().GoModuleDir, head, baseSHA)
			if err != nil {
				return RebuildDecision{}, err
			}
			findings.add(refusal)
			readable = refusal == nil
		}
		if err := recipeClause(); err != nil {
			return RebuildDecision{}, err
		}
		if len(changed) == 0 {
			// The toolchain-only delta, as for npm below.
			return decide(head, LockfileDelta{}), nil
		}
		authority := registryClause()
		if !readable {
			return decide(head, LockfileDelta{}), nil
		}
		delta, moduleFindings := evaluateGoModuleDelta(baseModule, headModule, authority)
		for _, finding := range moduleFindings {
			findings.add(&finding)
		}
		return decide(head, delta), nil
	}
	if len(changed) == 0 && !upkeep {
		return RebuildDecision{Head: head}, nil
	}
	if len(head.UnsupportedInputs) != 0 {
		findings.add(refuseLockfile(RebuildRefusalUnsupportedInput,
			"the candidate contains %s", strings.Join(head.UnsupportedInputs, ", ")))
	}
	if err := recipeClause(); err != nil {
		return RebuildDecision{}, err
	}
	if len(changed) == 0 {
		// The toolchain-only delta: the lockfile the image baked is the
		// tip's, so it has nothing for the registry or lockfile clauses.
		return decide(head, LockfileDelta{}), nil
	}
	authority := registryClause()
	delta, lockfileFindings, err := readLockfileDelta(ctx, reader, baseSHA, headSHA, authority)
	if err != nil {
		return RebuildDecision{}, err
	}
	for _, finding := range lockfileFindings {
		findings.add(&finding)
	}
	return decide(head, delta), nil
}

// readManifest reads one dependency manifest at a commit. An absent file is
// no content, not an error: it parses as no JSON object, or as a Go module
// with no requirements, and its reader refuses what that leaves under its
// own clause.
func readManifest(ctx context.Context, reader *verify.CommitReader, commitSHA, name string) ([]byte, error) {
	content, present, err := reader.ReadFile(ctx, commitSHA, name, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s at %s: %w", name, commitSHA, err)
	}
	if !present {
		return nil, nil
	}
	return content, nil
}

// readLockfileDelta reads both commits' npm manifests and measures the delta
// between them (evaluateLockfileDelta). A nil registries skips only the
// authority check.
func readLockfileDelta(
	ctx context.Context, reader *verify.CommitReader, baseSHA, headSHA string, registries *domain.RegistrySet,
) (LockfileDelta, gateFindings, error) {
	var files [4][]byte
	for index, file := range []struct{ commit, name string }{
		{baseSHA, "package.json"},
		{baseSHA, "package-lock.json"},
		{headSHA, "package.json"},
		{headSHA, "package-lock.json"},
	} {
		content, err := readManifest(ctx, reader, file.commit, file.name)
		if err != nil {
			return LockfileDelta{}, nil, err
		}
		files[index] = content
	}
	delta, findings := evaluateLockfileDelta(files[0], files[1], files[2], files[3], registries)
	return delta, findings, nil
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
// digest), the base image, the declared Go module directory, and the
// registry destination, which is parsed back out of the image's own
// reference. Only the commit, the tag, and the source
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
	if image.Environment != nil {
		request.GoModuleDir = image.Environment.GoModuleDir
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
