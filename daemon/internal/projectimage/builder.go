// Package projectimage builds and proves one managed repository's runtime
// image from its exact source commit and trusted verification recipe.
package projectimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	// PreparationPath is the fixed image-owned dependency hydration helper a
	// project-image-aware verification room runs before every recipe argv.
	PreparationPath = "/usr/local/bin/freeside-project-prepare"
	// NPMCachePath is the baked dependency material the positive and negative
	// proofs exercise.
	NPMCachePath = "/opt/freeside/npm-cache"

	maxRecipeBytes   = 1 << 20
	maxManifestBytes = 32 << 20
)

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageNamePattern  = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	refTagPattern     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	networkFailure    = regexp.MustCompile(`(?i)(EAI_AGAIN|ENOTFOUND|ECONNREFUSED|ETIMEDOUT|request to https://registry)`)
)

// ErrInvalidRequest marks malformed or mutually inconsistent builder inputs.
var ErrInvalidRequest = errors.New("invalid project-image build request")

// ErrProofFailed marks a build whose allowlist, offline, cache-dependency, or
// repository-identity proof did not establish the contract.
var ErrProofFailed = errors.New("project-image proof failed")

// Request binds one build to its repository, commit, recipe, approved base,
// and registry destination. BaseBuildRef is the local tag Apple container can
// use for FROM; the builder verifies it resolves to BaseImageRef's exact digest
// before the build and records only BaseImageRef in provenance.
type Request struct {
	Repository        string
	RepositoryID      int64
	CommitSHA         string
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

// Options configures the concrete Apple-container builder.
type Options struct {
	GitPath       string
	ContainerPath string
	TempDir       string
	Log           io.Writer
	Record        func(context.Context, domain.ProjectImage) error
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
	Fetch(context.Context, string, string, string) error
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
	ImageDigest  string
	BaseBuildRef string
	BaseDigest   string
	Repository   string
	RepositoryID int64
	CommitSHA    string
	RecipeDigest domain.Digest
}

type runSpec struct {
	ImageRef  string
	Workspace string
	Argv      []string
	MaskCache bool
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
		source:            gitSource{gitPath: git, runner: runner},
		resolver:          defaultRepositoryResolver(),
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
	if err := b.resolver.Verify(ctx, normalized.Repository, normalized.RepositoryID); err != nil {
		return domain.ProjectImage{}, err
	}
	scratch, err := os.MkdirTemp(b.tempDir, "freeside-project-image-*")
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("create project-image scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // private scratch, best-effort after all handles close

	repositoryDir := filepath.Join(scratch, "repository.git")
	if err := b.source.Fetch(ctx, normalized.Repository, normalized.CommitSHA, repositoryDir); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("materialize %s at %s: %w",
			normalized.Repository, normalized.CommitSHA, err)
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
	if err := b.resolver.Verify(ctx, normalized.Repository, normalized.RepositoryID); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("post-fetch repository identity: %w: %w", err, ErrProofFailed)
	}
	sourceDir := filepath.Join(scratch, "source")
	if err := b.source.Copy(ctx, repositoryDir, normalized.CommitSHA, sourceDir); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("materialize source tree: %w", err)
	}
	contextDir := filepath.Join(scratch, "context")
	if err := createBuildContext(contextDir, sourceDir, normalized, recipeDigest); err != nil {
		return domain.ProjectImage{}, err
	}

	baseDigest := imageDigest(normalized.BaseImageRef)
	observedBase, err := b.backend.ImageDigest(ctx, normalized.BaseBuildRef)
	if err != nil {
		return domain.ProjectImage{}, fmt.Errorf("inspect build-time base %q: %w", normalized.BaseBuildRef, err)
	}
	if observedBase != baseDigest {
		return domain.ProjectImage{}, fmt.Errorf(
			"build-time base %q resolves to %s, expected %s: %w",
			normalized.BaseBuildRef, observedBase, baseDigest, ErrProofFailed)
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
	provenance := provenanceSpec{
		ImageDigest:  localDigest,
		BaseBuildRef: privateBaseRef, BaseDigest: baseDigest,
		Repository:   normalized.Repository,
		RepositoryID: normalized.RepositoryID, CommitSHA: normalized.CommitSHA,
		RecipeDigest: recipeDigest,
	}
	if err := b.backend.CheckProvenance(ctx, localRef, provenance); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("local image provenance: %w: %w", err, ErrProofFailed)
	}
	if err := b.backend.RemoveImage(context.WithoutCancel(ctx), privateBaseRef); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("release private base reference: %w", err)
	}
	basePinned = false
	if err := b.backend.CheckAllowlist(ctx, localRef); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("local image allowlist: %w: %w", err, ErrProofFailed)
	}
	if err := b.proveOffline(ctx, repositoryDir, normalized.CommitSHA, localRef, recipe.Commands, scratch); err != nil {
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
	if err := ref.Validate(); err != nil || imageDigest(ref) != localDigest {
		return domain.ProjectImage{}, fmt.Errorf(
			"published reference %q does not preserve built digest %s: %w",
			ref, localDigest, ErrProofFailed)
	}
	if err := b.backend.CheckAllowlist(ctx, string(ref)); err != nil {
		return domain.ProjectImage{}, fmt.Errorf("published image allowlist: %w: %w", err, ErrProofFailed)
	}

	result, err = domain.NewProjectImage(domain.ProjectImageInput{
		Repository: normalized.Repository, RepositoryID: normalized.RepositoryID,
		CommitSHA: normalized.CommitSHA, RecipeDigest: recipeDigest,
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       normalized.BaseImageRef, ImageRef: ref,
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

func (b *Builder) proveOffline(
	ctx context.Context,
	sourceDir, commit, imageRef string,
	commands [][]string,
	scratch string,
) error {
	// A failed process result means something only after the runtime has proved
	// it can start this image under the same no-network topology.
	preflight, err := b.backend.Run(ctx, runSpec{ImageRef: imageRef, Argv: []string{"true"}})
	if err != nil {
		return fmt.Errorf("offline preflight: %w: %w", err, ErrProofFailed)
	}
	if preflight.ExitCode != 0 {
		return fmt.Errorf("offline preflight exited %d: %w", preflight.ExitCode, ErrProofFailed)
	}
	for index, argv := range commands {
		workspace := filepath.Join(scratch, fmt.Sprintf("positive-%d", index))
		if err := b.source.Copy(ctx, sourceDir, commit, workspace); err != nil {
			return fmt.Errorf("materialize positive workspace %d: %w", index, err)
		}
		preparation, err := b.backend.Run(ctx, runSpec{
			ImageRef: imageRef, Workspace: workspace, Argv: []string{PreparationPath},
		})
		if err != nil {
			return fmt.Errorf("hydrate positive workspace %d: %w: %w", index, err, ErrProofFailed)
		}
		if preparation.ExitCode != 0 {
			return fmt.Errorf("hydrate positive workspace %d exited %d (%s): %w",
				index, preparation.ExitCode, boundedOutput(preparation.Output), ErrProofFailed)
		}
		result, err := b.backend.Run(ctx, runSpec{
			ImageRef: imageRef, Workspace: workspace, Argv: append([]string{}, argv...),
		})
		if err != nil {
			return fmt.Errorf("recipe command %d %q: %w: %w", index, argv[0], err, ErrProofFailed)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("recipe command %d %q exited %d (%s): %w",
				index, argv[0], result.ExitCode, boundedOutput(result.Output), ErrProofFailed)
		}
	}

	negative := filepath.Join(scratch, "negative")
	if err := b.source.Copy(ctx, sourceDir, commit, negative); err != nil {
		return fmt.Errorf("materialize negative workspace: %w", err)
	}
	result, runErr := b.backend.Run(ctx, runSpec{
		ImageRef: imageRef, Workspace: negative,
		Argv: []string{"npm", "ci", "--ignore-scripts"}, MaskCache: true,
	})
	switch {
	case runErr != nil:
		return fmt.Errorf("cache-masked preparation could not execute: %w: %w", runErr, ErrProofFailed)
	case result.ExitCode == 0:
		return fmt.Errorf("preparation succeeded with %s masked: %w", NPMCachePath, ErrProofFailed)
	case !networkFailure.Match(result.Output):
		return fmt.Errorf("cache-masked preparation failed without a registry/network attempt (%s): %w",
			boundedOutput(result.Output), ErrProofFailed)
	}
	return nil
}

func validateRequest(request Request) (Request, verify.Recipe, domain.Digest, error) {
	if !repositoryPattern.MatchString(request.Repository) || strings.Contains(request.Repository, "..") {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("repository %q: %w", request.Repository, ErrInvalidRequest)
	}
	if request.RepositoryID <= 0 {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("repository id %d: %w", request.RepositoryID, ErrInvalidRequest)
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
	if request.BaseBuildRef == "" {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("base build ref is required: %w", ErrInvalidRequest)
	}
	if strings.HasPrefix(request.BaseBuildRef, "-") ||
		strings.ContainsAny(request.BaseBuildRef, " \t\r\n@") ||
		strings.Contains(request.BaseBuildRef, "://") {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("base build ref %q: %w",
			request.BaseBuildRef, ErrInvalidRequest)
	}
	if (request.Registry == "") == (request.LocalRegistryPort == 0) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf(
			"exactly one of registry or local registry port is required: %w", ErrInvalidRequest)
	}
	if request.Registry != "" &&
		(strings.HasPrefix(request.Registry, "-") ||
			strings.HasPrefix(request.Registry, "/") ||
			strings.HasSuffix(request.Registry, "/") ||
			strings.ContainsAny(request.Registry, " \t\r\n@") ||
			strings.Contains(request.Registry, "://")) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("registry %q: %w", request.Registry, ErrInvalidRequest)
	}
	if request.LocalRegistryPort != 0 &&
		(request.LocalRegistryPort < 1024 || request.LocalRegistryPort > 65535) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("local registry port %d: %w",
			request.LocalRegistryPort, ErrInvalidRequest)
	}
	if request.ImageName == "" {
		replacer := strings.NewReplacer("/", "-", "_", "-", ".", "-")
		request.ImageName = "freeside-project-" + replacer.Replace(strings.ToLower(request.Repository))
	}
	if !imageNamePattern.MatchString(request.ImageName) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("image name %q: %w", request.ImageName, ErrInvalidRequest)
	}
	if request.RefTag == "" {
		request.RefTag = "v1"
	}
	if !refTagPattern.MatchString(request.RefTag) {
		return Request{}, verify.Recipe{}, "", fmt.Errorf("reference tag %q: %w", request.RefTag, ErrInvalidRequest)
	}
	for _, dns := range request.DNS {
		if dns == "" || strings.HasPrefix(dns, "-") || strings.ContainsAny(dns, " \t\r\n") {
			return Request{}, verify.Recipe{}, "", fmt.Errorf("DNS server %q: %w", dns, ErrInvalidRequest)
		}
	}
	if request.BuildProxy != "" {
		proxy, err := url.Parse(request.BuildProxy)
		if err != nil || proxy.Scheme != "http" || proxy.Host == "" ||
			proxy.User != nil || (proxy.Path != "" && proxy.Path != "/") ||
			proxy.RawQuery != "" || proxy.Fragment != "" {
			return Request{}, verify.Recipe{}, "", fmt.Errorf("build proxy %q: %w",
				request.BuildProxy, ErrInvalidRequest)
		}
	}
	return request, recipe, verify.RecipeDigest(request.Recipe), nil
}

func createBuildContext(
	contextDir, sourceDir string,
	request Request,
	recipeDigest domain.Digest,
) error {
	for _, name := range []string{"npm-shrinkwrap.json", ".npmrc"} {
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
	if err := os.WriteFile(filepath.Join(contextDir, "recipe.json"), request.Recipe, 0o600); err != nil {
		return fmt.Errorf("write trusted recipe to build context: %w", err)
	}
	// Executable by design: the image invokes this fixed, builder-owned helper.
	if err := os.WriteFile(filepath.Join(contextDir, "prepare"), []byte(prepareScript), 0o700); err != nil { //nolint:gosec // G306
		return fmt.Errorf("write preparation helper: %w", err)
	}
	containerfile := renderContainerfile(request, recipeDigest)
	if err := os.WriteFile(filepath.Join(contextDir, "Containerfile"), []byte(containerfile), 0o600); err != nil {
		return fmt.Errorf("write generated Containerfile: %w", err)
	}
	return nil
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
	return len(digest) == len("sha256:")+64 &&
		strings.HasPrefix(digest, "sha256:") &&
		strings.IndexFunc(digest[len("sha256:"):], func(r rune) bool {
			return (r < '0' || r > '9') && (r < 'a' || r > 'f')
		}) == -1
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
