// Package projectimage builds and proves one managed repository's runtime
// image from its exact source commit and trusted verification recipe.
package projectimage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	// PreparationPath is the fixed image-owned dependency hydration helper a
	// project-image-aware verification room runs before every recipe argv.
	PreparationPath = "/usr/local/bin/freeside-project-prepare"
	// NPMCachePath is the baked dependency material the positive and negative
	// proofs exercise for an npm image.
	NPMCachePath = "/opt/freeside/npm-cache"
	// GoModuleCachePath is the baked module cache the proofs exercise for a
	// Go image. Only modules are baked; the build cache stays per room.
	GoModuleCachePath = "/opt/freeside/go-mod-cache"

	maxRecipeBytes   = 1 << 20
	maxManifestBytes = 32 << 20
)

// unsupportedInputNames are the npm inputs no project image supports. The
// build commit and every run base are held to the same list.
var unsupportedInputNames = []string{"npm-shrinkwrap.json", ".npmrc"}

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageNamePattern  = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	refTagPattern     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	networkFailure    = regexp.MustCompile(`(?i)(EAI_AGAIN|ENOTFOUND|ECONNREFUSED|ETIMEDOUT|request to https://registry)`)
	// goOfflineRefusal is Go's answer to a module missing from the cache
	// under the launcher's GOPROXY=off, or a network failure should a
	// download be attempted anyway.
	goOfflineRefusal = regexp.MustCompile(`(module lookup disabled by GOPROXY=off|dial tcp|no such host)`)
)

// ErrInvalidRequest marks malformed or mutually inconsistent builder inputs.
var ErrInvalidRequest = errors.New("invalid project-image build request")

// ErrProofFailed marks a build whose allowlist, offline, cache-dependency, or
// repository-identity proof ran and did not establish the contract. A
// container-runtime command that failed inside a proof is not one: the proof
// produced no result, so the error carries neither this class nor a verdict.
var ErrProofFailed = errors.New("project-image proof failed")

// ErrInstallFailed marks a build whose dependency install failed in a way only
// the candidate's own manifests can cause, such as a tarball that doesn't
// match its lockfile integrity. Its error text leads with the installer's
// lines. An install failure the builder doesn't recognize doesn't carry it.
var ErrInstallFailed = errors.New("project-image dependency install failed")

// Request binds one build to its repository, commit, recipe, approved base,
// and registry destination. BaseBuildRef is the local tag Apple container can
// use for FROM; the builder verifies it resolves to BaseImageRef's exact digest
// before the build and records only BaseImageRef in provenance.
type Request struct {
	Repository   string
	RepositoryID int64
	CommitSHA    string
	// SourceDir, when set, is a local git checkout the build reads CommitSHA
	// from instead of cloning Repository from the forge. The builder then
	// makes no forge request and proves nothing about the checkout: the
	// caller owns proving it is Repository at RepositoryID, which the record
	// still names. The commit is materialized exactly, as from a clone.
	SourceDir string
	// GoModuleDir declares the Go module the recipe verifies, relative to
	// the repository root. Empty with a root package.json selects npm, empty
	// with a root go.mod and no package.json selects Go at the root, and any
	// other value selects Go at that directory.
	GoModuleDir       string
	Recipe            []byte
	BaseImageRef      domain.ImageRef
	BaseBuildRef      string
	Registry          string
	LocalRegistryPort int
	ImageName         string
	RefTag            string
	DNS               []string
	BuildProxy        string
}

// NormalizeRequest applies the builder's stable defaults so callers can bind
// approval and provenance to the exact request the builder will execute.
func NormalizeRequest(request Request) Request {
	if request.ImageName == "" {
		replacer := strings.NewReplacer("/", "-", "_", "-", ".", "-")
		request.ImageName = "freeside-project-" + replacer.Replace(strings.ToLower(request.Repository))
	}
	if request.RefTag == "" {
		request.RefTag = "v1"
	}
	return request
}

// Options configures the concrete Apple-container builder.
type Options struct {
	GitPath       string
	ContainerPath string
	TempDir       string
	Log           io.Writer
	// Tokens authenticates private-repository identity and clone requests.
	// It must return the exact read-only onboarding permission set; nil keeps
	// the standalone public-repository builder anonymous.
	Tokens publish.TokenSource
	Record func(context.Context, domain.ProjectImage) error
	// LookupRecordedRef reports whether a published image reference already
	// backs a durably recorded ProjectImage row. Failure-path residue
	// removal consults it before deleting, so a rebuild that reproduces an
	// already-recorded digest cannot destroy the prior row's artifact; nil
	// skips the guard (no store implies no rows to protect).
	LookupRecordedRef func(context.Context, string) (bool, error)
}

// Builder owns source materialization and image-runtime orchestration. The
// interfaces are deliberately narrow so unit tests can refute sequencing and
// returned-object trust without invoking the host runtime.
type Builder struct {
	source            source
	resolver          repositoryResolver
	backend           backend
	record            func(context.Context, domain.ProjectImage) error
	lookupRecordedRef func(context.Context, string) (bool, error)
	tempDir           string
	log               io.Writer
}

type source interface {
	Fetch(context.Context, string, int64, string, string) error
	Copy(context.Context, string, string, string) error
}

type backend interface {
	ImageDigest(context.Context, string) (string, error)
	PinBase(context.Context, string, string, string) error
	RemoveImage(context.Context, string) error
	Build(context.Context, buildSpec) error
	CheckProvenance(context.Context, string, provenanceSpec) error
	CheckAllowlist(context.Context, string) error
	Run(context.Context, runSpec) (runResult, error)
	Publish(context.Context, publishSpec) (publication, error)
}

type buildSpec struct {
	ContextDir   string
	LocalRef     string
	BaseRef      string
	BaseDigest   string
	Repository   string
	RepositoryID int64
	CommitSHA    string
	RecipeDigest domain.Digest
	DNS          []string
	BuildProxy   string
}

type provenanceSpec struct {
	ImageDigest                string
	BaseBuildRef               string
	BaseDigest                 string
	Repository                 string
	RepositoryID               int64
	CommitSHA                  string
	RecipeDigest               domain.Digest
	Layout                     domain.ProjectImageInputLayout
	NodeVersion                string
	NodeToolchainArchiveSHA256 string
	PackageJSONSHA256          string
	PackageLockSHA256          string
	GoVersion                  string
	GoToolchainArchiveSHA256   string
	GolangciLintVersion        string
	GolangciLintArchiveSHA256  string
	VacuumVersion              string
	VacuumArchiveSHA256        string
	ShellcheckVersion          string
	ShellcheckArchiveSHA256    string
	PythonVersion              string
	PythonArchiveSHA256        string
	ProcpsVersion              string
	ProcpsArchiveSHA256        string
	Libproc2ArchiveSHA256      string
	GoModSHA256                string
	GoSumSHA256                string
}

type runSpec struct {
	ImageRef  string
	Workspace string
	Argv      []string
	// MaskedCache is the image path an empty tmpfs covers for this run, so
	// the run cannot read the baked dependency cache; empty masks nothing.
	MaskedCache string
}

type runResult struct {
	Output    []byte
	ExitCode  int
	Truncated bool
}

type publishSpec struct {
	LocalRef          string
	Digest            string
	ImageName         string
	RefTag            string
	Registry          string
	LocalRegistryPort int
	RefRecorded       func(context.Context, string) (bool, error)
}

type publication struct {
	Ref     domain.ImageRef
	cleanup func(context.Context) error
	discard func(context.Context) error
	release func() error
}

// New constructs the production builder backed by git and Apple container.
func New(opts Options) (*Builder, error) {
	runner := execRunner{}
	git, err := resolveExecutable(opts.GitPath, "git")
	if err != nil {
		return nil, err
	}
	container, err := resolveExecutable(opts.ContainerPath, "container")
	if err != nil {
		return nil, err
	}
	if opts.Record == nil {
		return nil, fmt.Errorf("project image recorder is required: %w", ErrInvalidRequest)
	}
	return &Builder{
		source:            gitSource{gitPath: git, runner: runner, tokens: opts.Tokens},
		resolver:          defaultRepositoryResolver(opts.Tokens),
		backend:           appleBackend{containerPath: container, runner: runner},
		record:            opts.Record,
		lookupRecordedRef: opts.LookupRecordedRef,
		tempDir:           opts.TempDir,
		log:               opts.Log,
	}, nil
}

func newBuilder(src source, images backend, tempDir string) *Builder {
	return &Builder{
		source: src, resolver: trustedRepositoryResolver{},
		backend: images, record: func(context.Context, domain.ProjectImage) error {
			return nil
		}, tempDir: tempDir,
	}
}

// Build derives, proves, publishes, and returns one immutable project image.
func (b *Builder) Build(
	ctx context.Context,
	request Request,
) (result domain.ProjectImage, err error) {
	normalized, recipe, recipeDigest, err := validateRequest(request)
	if err != nil {
		return domain.ProjectImage{}, err
	}
	scratch, err := os.MkdirTemp(b.tempDir, "freeside-project-image-*")
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("create project-image scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // private scratch, best-effort after all handles close

	repositoryDir, err := b.materializeRepository(ctx, normalized, scratch)
	if err != nil {
		return domain.ProjectImage{}, err
	}
	sourceDir := filepath.Join(scratch, "source")
	if err := b.source.Copy(ctx, repositoryDir, normalized.CommitSHA, sourceDir); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("materialize source tree: %w", err)
	}
	contextDir := filepath.Join(scratch, "context")
	layout, err := createBuildContext(contextDir, sourceDir, normalized, recipeDigest)
	if err != nil {
		return domain.ProjectImage{}, err
	}
	environment, err := bakedEnvironment(contextDir, layout)
	if err != nil {
		return domain.ProjectImage{}, err
	}

	baseDigest := imageDigest(normalized.BaseImageRef)
	observedBase, err := b.backend.ImageDigest(ctx, normalized.BaseBuildRef)
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("inspect build-time base %q: %w", normalized.BaseBuildRef, err)
	}
	// The base tag is the daemon's configuration, not the candidate's, so a
	// drifted tag is not a failed proof.
	if observedBase != baseDigest {
		return domain.ProjectImage{}, fmt.Errorf(
			"build-time base %q resolves to %s, expected %s",
			normalized.BaseBuildRef, observedBase, baseDigest)
	}
	baseToken, err := randomToken()
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("generate private base reference: %w", err)
	}
	privateBaseRef := normalized.ImageName + ":freeside-base-" + baseToken
	if err := b.backend.PinBase(
		ctx, normalized.BaseBuildRef, privateBaseRef, baseDigest,
	); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("pin build-time base: %w", err)
	}
	basePinned := true
	defer func() {
		if !basePinned {
			return
		}
		if releaseErr := b.backend.RemoveImage(context.WithoutCancel(ctx), privateBaseRef); releaseErr != nil {
			result = domain.ProjectImage{}
			err = errors.Join(err, fmt.Errorf("release private base reference: %w", releaseErr))
		}
	}()

	localRef := normalized.ImageName + ":" + filepath.Base(scratch) + "-" + strconv.Itoa(os.Getpid())
	b.logf("building %s from %s at %s", localRef, normalized.Repository, normalized.CommitSHA)
	if err := b.backend.Build(ctx, buildSpec{
		ContextDir: contextDir, LocalRef: localRef,
		BaseRef: privateBaseRef, BaseDigest: baseDigest,
		Repository: normalized.Repository, RepositoryID: normalized.RepositoryID,
		CommitSHA: normalized.CommitSHA, RecipeDigest: recipeDigest,
		DNS:        append([]string{}, normalized.DNS...),
		BuildProxy: normalized.BuildProxy,
	}); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("build project image: %w", err)
	}
	defer func() {
		if removeErr := b.backend.RemoveImage(context.WithoutCancel(ctx), localRef); removeErr != nil {
			result = domain.ProjectImage{}
			err = errors.Join(err, fmt.Errorf("remove local project image: %w", removeErr))
		}
	}()
	localDigest, err := b.backend.ImageDigest(ctx, localRef)
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("inspect built image: %w", err)
	}
	if !validDigest(localDigest) {
		return domain.ProjectImage{}, fmt.Errorf("built image reported digest %q: %w", localDigest, ErrProofFailed)
	}
	provenance := expectedProvenance(provenanceSpec{
		ImageDigest:  localDigest,
		BaseBuildRef: privateBaseRef, BaseDigest: baseDigest,
		Repository:   normalized.Repository,
		RepositoryID: normalized.RepositoryID, CommitSHA: normalized.CommitSHA,
		RecipeDigest: recipeDigest, Layout: layout,
	}, environment)
	if err := b.backend.CheckProvenance(ctx, localRef, provenance); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("local image provenance: %w", proofFailure(err))
	}
	if err := b.backend.RemoveImage(context.WithoutCancel(ctx), privateBaseRef); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("release private base reference: %w", err)
	}
	basePinned = false
	if err := b.backend.CheckAllowlist(ctx, localRef); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("local image allowlist: %w", proofFailure(err))
	}
	if err := b.proveOffline(
		ctx, repositoryDir, normalized.CommitSHA, localRef, recipe.Commands, scratch, layout,
	); err != nil {
		return domain.ProjectImage{}, err
	}

	published, err := b.backend.Publish(ctx, publishSpec{
		LocalRef: localRef, Digest: localDigest, ImageName: normalized.ImageName,
		RefTag: normalized.RefTag, Registry: normalized.Registry,
		LocalRegistryPort: normalized.LocalRegistryPort,
		RefRecorded:       b.lookupRecordedRef,
	})
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("publish project image: %w", err)
	}
	// Register the lease release before cleanup so deferred LIFO order keeps
	// the same-port exclusion in force until any owned registry is deleted.
	defer func() {
		if published.release == nil {
			return
		}
		if releaseErr := published.release(); releaseErr != nil {
			result = domain.ProjectImage{}
			err = errors.Join(err, fmt.Errorf("release project-image publication: %w", releaseErr))
		}
	}()
	defer func() {
		if published.cleanup == nil {
			return
		}
		if cleanupErr := published.cleanup(context.WithoutCancel(ctx)); cleanupErr != nil {
			result = domain.ProjectImage{}
			err = errors.Join(err, fmt.Errorf("clean unpublished project image: %w", cleanupErr))
		}
	}()
	// Registered after cleanup so LIFO order discards residue while any
	// owned registry is still alive and the same-port lease is still held;
	// unlike cleanup, discard also runs for a reused registry this
	// invocation does not own.
	defer func() {
		if published.discard == nil {
			return
		}
		if discardErr := published.discard(context.WithoutCancel(ctx)); discardErr != nil {
			result = domain.ProjectImage{}
			err = errors.Join(err, fmt.Errorf("discard unpublished project image: %w", discardErr))
		}
	}()
	ref := published.Ref
	if err := ValidatePublishedRef(normalized, ref); err != nil ||
		imageDigest(ref) != localDigest {
		return domain.ProjectImage{}, fmt.Errorf(
			"published reference %q does not match the approved destination and built digest %s: %w",
			ref, localDigest, ErrProofFailed)
	}
	if err := b.backend.CheckAllowlist(ctx, string(ref)); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("published image allowlist: %w", proofFailure(err))
	}

	result, err = domain.NewProjectImage(domain.ProjectImageInput{
		Repository: normalized.Repository, RepositoryID: normalized.RepositoryID,
		CommitSHA: normalized.CommitSHA, RecipeDigest: recipeDigest,
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       normalized.BaseImageRef, ImageRef: ref,
		Environment: &environment,
	})
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("construct project-image result: %w", err)
	}
	if err := b.record(ctx, result); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("record project image: %w", err)
	}
	published.cleanup = nil
	published.discard = nil
	return result, nil
}

// materializeRepository returns the git repository the build reads its commit
// from: the caller's checkout for a local-source request, a fresh clone
// otherwise.
func (b *Builder) materializeRepository(
	ctx context.Context, request Request, scratch string,
) (string, error) {
	if request.SourceDir != "" {
		// No forge request and no identity check: the name-to-ID binding the
		// clone path proves below is about a name-addressed fetch, and
		// nothing is fetched here.
		return request.SourceDir, nil
	}
	if err := b.resolver.Verify(ctx, request.Repository, request.RepositoryID); err != nil {
		return "", err
	}
	repositoryDir := filepath.Join(scratch, "repository.git")
	if err := b.source.Fetch(
		ctx, request.Repository, request.RepositoryID, request.CommitSHA, repositoryDir,
	); err != nil {
		return "", fmt.Errorf("materialize %s at %s: %w", request.Repository, request.CommitSHA, err)
	}
	// Re-resolve owner/name -> numeric ID now that the clone is complete: the
	// HTTPS clone URL is name-addressed and mutable, and forks share object
	// stores (see ward's seed rebinding), so a name transferred between the
	// pre-fetch verification and the clone would serve foreign content that
	// still carries the pinned commit. Verifying at both edges of the fetch
	// rebinds the fetched content to the pre-verified RepositoryID; a transfer
	// away and back inside the clone window, or API state lagging git serving,
	// still escapes, and GitHub offers no ID-bound fetch mechanism to close
	// either.
	if err := b.resolver.Verify(ctx, request.Repository, request.RepositoryID); err != nil {
		return "", fmt.Errorf("post-fetch repository identity: %w: %w", err, ErrProofFailed)
	}
	return repositoryDir, nil
}

// ValidatePublishedRef proves that ref names the exact registry and image
// destination selected by request. A digest-pinned reference under a different
// destination is a different artifact, even when every build input matches.
func ValidatePublishedRef(request Request, ref domain.ImageRef) error {
	normalized, _, _, err := validateRequest(request)
	if err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("published reference %q: %w", ref, ErrProofFailed)
	}
	registry := strings.TrimSuffix(normalized.Registry, "/")
	if normalized.LocalRegistryPort != 0 {
		registry = "127.0.0.1:" + strconv.Itoa(normalized.LocalRegistryPort)
	}
	repository, _, ok := strings.Cut(string(ref), "@")
	expected := registry + "/" + normalized.ImageName
	if !ok || repository != expected {
		return fmt.Errorf(
			"published reference %q targets %q, want %q: %w",
			ref, repository, expected, ErrProofFailed,
		)
	}
	return nil
}

// expectedProvenance completes spec with the layout toolchain's pins and the
// baked manifest hashes. It leaves the other toolchain's fields empty, so a
// spec never claims a toolchain the image does not carry.
func expectedProvenance(spec provenanceSpec, environment domain.ProjectImageEnvironment) provenanceSpec {
	switch spec.Layout.Toolchain {
	case domain.ProjectToolchainNPM:
		spec.NodeVersion = nodeToolchainVersion
		spec.NodeToolchainArchiveSHA256 = nodeToolchainArchiveSHA256
		spec.PackageJSONSHA256 = environment.PackageJSONSHA256
		spec.PackageLockSHA256 = environment.PackageLockSHA256
	case domain.ProjectToolchainGo:
		spec.GoVersion = goToolchainVersion
		spec.GoToolchainArchiveSHA256 = goToolchainArchiveSHA256
		spec.GolangciLintVersion = golangciLintVersion
		spec.GolangciLintArchiveSHA256 = golangciLintArchiveSHA256
		spec.VacuumVersion = vacuumVersion
		spec.VacuumArchiveSHA256 = vacuumArchiveSHA256
		spec.ShellcheckVersion = shellcheckVersion
		spec.ShellcheckArchiveSHA256 = shellcheckArchiveSHA256
		spec.PythonVersion = pythonVersion
		spec.PythonArchiveSHA256 = pythonArchiveSHA256
		spec.ProcpsVersion = procpsVersion
		spec.ProcpsArchiveSHA256 = procpsArchiveSHA256
		spec.Libproc2ArchiveSHA256 = libproc2ArchiveSHA256
		spec.GoModSHA256 = environment.GoModSHA256
		spec.GoSumSHA256 = environment.GoSumSHA256
	}
	return spec
}

// maskedProbe names a layout's cache-masked negative probe: the argv that
// hydrates the workspace's dependencies, the baked cache the probe masks,
// and the output that shows the hydration reached for the network.
func maskedProbe(layout domain.ProjectImageInputLayout) ([]string, string, *regexp.Regexp) {
	switch layout.Toolchain {
	case domain.ProjectToolchainNPM:
		return []string{"npm", "ci", "--ignore-scripts"}, NPMCachePath, networkFailure
	case domain.ProjectToolchainGo:
		return goMaskedProbe(layout.GoModuleDir), GoModuleCachePath, goOfflineRefusal
	}
	return nil, "", nil
}

func (b *Builder) proveOffline(
	ctx context.Context,
	sourceDir, commit, imageRef string,
	commands [][]string,
	scratch string,
	layout domain.ProjectImageInputLayout,
) error {
	probe, cache, networkAttempt := maskedProbe(layout)
	// A failed process result means something only after the runtime has proved
	// it can start this image under the same no-network topology. The
	// preflight proves nothing about the candidate: `true` comes from the
	// approved base's layers, which the candidate's dependencies only add files
	// beside, and provenance has already bounded those layers. So a failure
	// here, like a runtime error anywhere below, is not a failed proof.
	preflight, err := b.backend.Run(ctx, runSpec{ImageRef: imageRef, Argv: []string{"true"}})
	if err != nil {
		return fmt.Errorf("offline preflight: %w", err)
	}
	if preflight.ExitCode != 0 {
		return fmt.Errorf("offline preflight exited %d", preflight.ExitCode)
	}
	if err := b.proveRecipe(ctx, sourceDir, commit, imageRef, commands, scratch, ""); err != nil {
		return err
	}

	negative := filepath.Join(scratch, "negative")
	if err := b.source.Copy(ctx, sourceDir, commit, negative); err != nil {
		return fmt.Errorf("materialize negative workspace: %w", err)
	}
	result, runErr := b.backend.Run(ctx, runSpec{
		ImageRef: imageRef, Workspace: negative,
		Argv: probe, MaskedCache: cache,
	})
	switch {
	case runErr != nil:
		return fmt.Errorf("cache-masked preparation could not execute: %w", runErr)
	case result.ExitCode == 0:
		// Hydration need not consume the cache, for example with no
		// dependencies. Prove the complete recipe works without it rather than
		// infer this from manifest shape or accept hydration alone as
		// sufficient proof.
		if err := b.proveRecipe(ctx, sourceDir, commit, imageRef, commands, scratch, cache); err != nil {
			return fmt.Errorf("cache-independent verification: %w", err)
		}
	case !networkAttempt.Match(result.Output):
		return fmt.Errorf("cache-masked preparation failed without a registry/network attempt (%s): %w",
			boundedOutput(result.Output), ErrProofFailed)
	}
	return nil
}

func (b *Builder) proveRecipe(
	ctx context.Context,
	sourceDir, commit, imageRef string,
	commands [][]string,
	scratch string,
	maskedCache string,
) error {
	prefix := "positive"
	if maskedCache != "" {
		prefix = "cache-masked"
	}
	for index, argv := range commands {
		workspace := filepath.Join(scratch, fmt.Sprintf("%s-%d", prefix, index))
		if err := b.source.Copy(ctx, sourceDir, commit, workspace); err != nil {
			return fmt.Errorf("materialize positive workspace %d: %w", index, err)
		}
		preparation, err := b.backend.Run(ctx, runSpec{
			ImageRef: imageRef, Workspace: workspace, Argv: []string{PreparationPath}, MaskedCache: maskedCache,
		})
		if err != nil {
			return fmt.Errorf("hydrate positive workspace %d: %w", index, err)
		}
		if preparation.ExitCode != 0 {
			return fmt.Errorf("hydrate positive workspace %d exited %d (%s): %w",
				index, preparation.ExitCode, boundedOutput(preparation.Output), ErrProofFailed)
		}
		result, err := b.backend.Run(ctx, runSpec{
			ImageRef: imageRef, Workspace: workspace, Argv: append([]string{}, argv...), MaskedCache: maskedCache,
		})
		if err != nil {
			return fmt.Errorf("recipe command %d %q: %w", index, argv[0], err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("recipe command %d %q exited %d (%s): %w",
				index, argv[0], result.ExitCode, boundedOutput(result.Output), ErrProofFailed)
		}
	}
	return nil
}

// proofFailure classes a provenance or allowlist check's error as a failed
// proof unless a container-runtime command the check needed failed, in which
// case the check produced no result. A check whose result failed and whose
// cleanup then hit a runtime fault carries both, and the fault wins: the
// build retries rather than record a verdict the runtime clouded.
func proofFailure(err error) error {
	if isRuntimeFault(err) {
		return err
	}
	return fmt.Errorf("%w: %w", err, ErrProofFailed)
}

// ValidateRegistry refuses a registry destination the builder cannot publish
// to. Every refusal wraps ErrInvalidRequest. A loopback host (localhost, any
// 127.0.0.0/8 address, or ::1) is refused because -local-registry-port is the
// supported loopback path: it leases the port, checks the retained registry's
// ownership, and publishes over HTTP, while -registry mode does none of that.
// The check is syntactic; a name that only resolves to loopback passes.
func ValidateRegistry(registry string) error {
	if strings.HasPrefix(registry, "-") ||
		strings.HasPrefix(registry, "/") ||
		strings.HasSuffix(registry, "/") ||
		strings.ContainsAny(registry, " \t\r\n@") ||
		strings.Contains(registry, "://") {
		return fmt.Errorf("registry %q: %w", registry, ErrInvalidRequest)
	}
	hostPort, _, _ := strings.Cut(registry, "/")
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(hostPort, "["), "]")
	}
	if ip := net.ParseIP(host); strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		return fmt.Errorf("registry %q is a loopback host; pass the port as -local-registry-port instead: %w",
			registry, ErrInvalidRequest)
	}
	return nil
}

func validateRequest(request Request) (Request, verify.Recipe, domain.Digest, error) {
	request = NormalizeRequest(request)
	if !repositoryPattern.MatchString(request.Repository) || strings.Contains(request.Repository, "..") {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("repository %q: %w", request.Repository, ErrInvalidRequest)
	}
	if request.RepositoryID <= 0 {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("repository id %d: %w", request.RepositoryID, ErrInvalidRequest)
	}
	if !domain.ValidProjectGoModuleDir(request.GoModuleDir) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("go module directory %q: %w", request.GoModuleDir, ErrInvalidRequest)
	}
	if !commitPattern.MatchString(request.CommitSHA) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("commit %q is not a full lowercase SHA-1: %w",
			request.CommitSHA, ErrInvalidRequest)
	}
	if len(request.Recipe) == 0 || len(request.Recipe) > maxRecipeBytes {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("recipe size %d: %w", len(request.Recipe), ErrInvalidRequest)
	}
	recipe, err := verify.ParseRecipe(request.Recipe)
	if err != nil {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("trusted recipe: %w: %w", err, ErrInvalidRequest)
	}
	if err := request.BaseImageRef.Validate(); err != nil {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("base image: %w: %w", err, ErrInvalidRequest)
	}
	if err := validateBaseBuildRef(request.BaseBuildRef); err != nil {
		return Request{}, verify.Recipe{}, "", err
	}
	if (request.Registry == "") == (request.LocalRegistryPort == 0) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf(
			"exactly one of registry or local registry port is required: %w", ErrInvalidRequest)
	}
	if request.Registry != "" {
		if err := ValidateRegistry(request.Registry); err != nil {
			return Request{}, verify.Recipe{}, "", err
		}
	}
	if request.LocalRegistryPort != 0 &&
		(request.LocalRegistryPort < 1024 || request.LocalRegistryPort > 65535) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("local registry port %d: %w",
			request.LocalRegistryPort, ErrInvalidRequest)
	}
	if !imageNamePattern.MatchString(request.ImageName) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("image name %q: %w", request.ImageName, ErrInvalidRequest)
	}
	if !refTagPattern.MatchString(request.RefTag) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("reference tag %q: %w", request.RefTag, ErrInvalidRequest)
	}
	if err := validateBuildDNS(request.DNS); err != nil {
		return Request{}, verify.Recipe{}, "", err
	}
	if err := ValidateBuildProxy(request.BuildProxy); err != nil {
		return Request{}, verify.Recipe{}, "", err
	}
	return request, recipe, verify.RecipeDigest(request.Recipe), nil
}

func validateBaseBuildRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("base build ref is required: %w", ErrInvalidRequest)
	}
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\r\n@") || strings.Contains(ref, "://") {
		return fmt.Errorf("base build ref %q: %w", ref, ErrInvalidRequest)
	}
	return nil
}

func validateBuildDNS(servers []string) error {
	for _, dns := range servers {
		if dns == "" || strings.HasPrefix(dns, "-") || strings.ContainsAny(dns, " \t\r\n") {
			return fmt.Errorf("DNS server %q: %w", dns, ErrInvalidRequest)
		}
	}
	return nil
}

// ValidateBuildProxy validates the supported project-image egress proxy
// shape without attempting a connection or changing runtime state.
func ValidateBuildProxy(raw string) error {
	if raw == "" {
		return nil
	}
	proxy, err := url.Parse(raw)
	if err != nil || proxy.Scheme != "http" || proxy.Host == "" ||
		proxy.User != nil || (proxy.Path != "" && proxy.Path != "/") ||
		proxy.RawQuery != "" || proxy.Fragment != "" {
		return fmt.Errorf("build proxy %q: %w", raw, ErrInvalidRequest)
	}
	return nil
}

// createBuildContext selects the source's input layout, refuses inputs the
// layout's toolchain does not support, and writes the build context: the
// dependency manifests, the trusted recipe, and the toolchain's fixed
// preparation helper, launcher, and Containerfile.
func createBuildContext(
	contextDir, sourceDir string,
	request Request,
	recipeDigest domain.Digest,
) (domain.ProjectImageInputLayout, error) {
	layout, err := selectInputLayout(sourceDir, request.GoModuleDir)
	if err != nil {
		return domain.ProjectImageInputLayout{}, err
	}
	var preparation, launcher string
	switch layout.Toolchain {
	case domain.ProjectToolchainNPM:
		preparation, launcher = prepareScript, nodeToolchainLauncher
		err = stageNPMInputs(contextDir, sourceDir)
	case domain.ProjectToolchainGo:
		preparation, launcher = goPrepareScript(layout.GoModuleDir), goToolchainLauncher
		err = stageGoInputs(contextDir, sourceDir, layout.GoModuleDir)
	}
	if err != nil {
		return domain.ProjectImageInputLayout{}, err
	}
	if err := os.WriteFile(filepath.Join(contextDir, "recipe.json"), request.Recipe, 0o600); err != nil {
		return domain.ProjectImageInputLayout{}, fmt.Errorf("write trusted recipe to build context: %w", err)
	}
	// Executable by design: the image invokes this fixed, builder-owned helper.
	if err := os.WriteFile(filepath.Join(contextDir, "prepare"), []byte(preparation), 0o700); err != nil { //nolint:gosec // G306
		return domain.ProjectImageInputLayout{}, fmt.Errorf("write preparation helper: %w", err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "toolchain-launcher"), []byte(launcher), 0o755); err != nil { //nolint:gosec // G306: executable fixed builder-owned helper
		return domain.ProjectImageInputLayout{}, fmt.Errorf("write toolchain launcher: %w", err)
	}
	// WriteFile's mode is filtered by the caller's umask; image provenance
	// requires these fixed launcher bytes to be executable with exactly 0755.
	if err := os.Chmod(filepath.Join(contextDir, "toolchain-launcher"), 0o755); err != nil { //nolint:gosec // G302: public fixed launcher inside the private build context
		return domain.ProjectImageInputLayout{}, fmt.Errorf("set toolchain launcher mode: %w", err)
	}
	containerfile := renderContainerfile(layout, request, recipeDigest)
	if err := os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte(containerfile), 0o600); err != nil {
		return domain.ProjectImageInputLayout{}, fmt.Errorf("write generated Containerfile: %w", err)
	}
	return layout, nil
}

// selectInputLayout decides which toolchain builds the source. A declared
// module directory selects Go there and refuses a root package.json, since
// one image carries one toolchain. Without one, a root package.json keeps
// the npm path every existing image takes, and only a tree with no
// package.json but a root go.mod selects Go at the root; a tree with
// neither takes the npm path and fails on its missing manifest as before.
func selectInputLayout(sourceDir, goModuleDir string) (domain.ProjectImageInputLayout, error) {
	if !domain.ValidProjectGoModuleDir(goModuleDir) {
		return domain.ProjectImageInputLayout{}, fmt.Errorf("go module directory %q: %w", goModuleDir, ErrInvalidRequest)
	}
	packageJSON, err := entryExists(filepath.Join(sourceDir, "package.json"))
	if err != nil {
		return domain.ProjectImageInputLayout{}, fmt.Errorf("inspect package.json: %w", err)
	}
	goLayout := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainGo, GoModuleDir: goModuleDir}
	if goModuleDir != "" {
		if packageJSON {
			return domain.ProjectImageInputLayout{}, fmt.Errorf(
				"source contains a root package.json beside the declared Go module %s; an image carries one toolchain",
				goModuleDir)
		}
		return goLayout, nil
	}
	if packageJSON {
		return domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}, nil
	}
	goMod, err := entryExists(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		return domain.ProjectImageInputLayout{}, fmt.Errorf("inspect go.mod: %w", err)
	}
	if goMod {
		return goLayout, nil
	}
	return domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}, nil
}

// entryExists reports whether path names any directory entry, a dangling
// symlink included.
func entryExists(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

func stageNPMInputs(contextDir, sourceDir string) error {
	for _, name := range unsupportedInputNames {
		if _, err := os.Lstat(filepath.Join(sourceDir, name)); err == nil {
			return fmt.Errorf("source contains unsupported npm input %s", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect unsupported npm input %s: %w", name, err)
		}
	}
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		return fmt.Errorf("create image build context: %w", err)
	}
	for _, name := range []string{"package.json", "package-lock.json"} {
		if err := copyRegularFile(
			filepath.Join(sourceDir, name), filepath.Join(contextDir, name), maxManifestBytes,
		); err != nil {
			return fmt.Errorf("copy %s from exact commit: %w", name, err)
		}
	}
	return nil
}

// bakedEnvironment records the environment inputs the build context carries:
// the layout, the exact manifest bytes COPY will bake as the dependency seed,
// and this binary's preparation implementation. It hashes the context's
// copies, not the source tree's, so the record describes what the image is
// built from. An npm environment leaves the toolchain empty, as every record
// written before the Go path did.
func bakedEnvironment(
	contextDir string, layout domain.ProjectImageInputLayout,
) (domain.ProjectImageEnvironment, error) {
	environment := domain.ProjectImageEnvironment{PreparationDigest: PreparationDigest()}
	var manifests map[string]*string
	switch layout.Toolchain {
	case domain.ProjectToolchainNPM:
		manifests = map[string]*string{
			"package.json":      &environment.PackageJSONSHA256,
			"package-lock.json": &environment.PackageLockSHA256,
		}
	case domain.ProjectToolchainGo:
		environment.Toolchain = domain.ProjectToolchainGo
		environment.GoModuleDir = layout.GoModuleDir
		manifests = map[string]*string{
			"go.mod": &environment.GoModSHA256,
			"go.sum": &environment.GoSumSHA256,
		}
	}
	for name, hash := range manifests {
		content, err := os.ReadFile(filepath.Join(contextDir, name)) //nolint:gosec // G304: fixed manifest name inside the private build context
		if err != nil {
			return domain.ProjectImageEnvironment{}, fmt.Errorf("hash baked %s: %w", name, err)
		}
		*hash = manifestSHA256(content)
	}
	return environment, nil
}

func manifestSHA256(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

func copyRegularFile(source, target string, maxBytes int64) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file")
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("source exceeds %d-byte cap", maxBytes)
	}
	in, err := os.Open(source) //nolint:gosec // G304: source is a fixed manifest name under the exact materialized commit
	if err != nil {
		return err
	}
	defer in.Close()                                                          //nolint:errcheck // read-only source
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: target is the private generated context plus a fixed name
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, io.LimitReader(in, maxBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxBytes {
		return fmt.Errorf("source grew beyond %d-byte cap", maxBytes)
	}
	return nil
}

func imageDigest(ref domain.ImageRef) string {
	_, digest, found := strings.Cut(string(ref), "@")
	if !found {
		return ""
	}
	return digest
}

func validDigest(digest string) bool {
	return contentaddr.Valid(digest)
}

func boundedOutput(output []byte) string {
	const limit = 2048
	if len(output) > limit {
		output = output[len(output)-limit:]
	}
	return strings.TrimSpace(string(output))
}

func (b *Builder) logf(format string, args ...any) {
	if b.log != nil {
		_, _ = fmt.Fprintf(b.log, "project-image: "+format+"\n", args...)
	}
}
