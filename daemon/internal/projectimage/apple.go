package projectimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"golang.org/x/sys/unix"
)

const (
	registryImageDigest = "sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	registryImage       = "docker.io/library/registry@" + registryImageDigest
	ownershipLabel      = "ai.freeside.project-image.owner"
	registryPortLabel   = "ai.freeside.project-image.registry-port"
	allowlistPathEnv    = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	busyboxPath         = "/usr/bin/busybox"
	maxPrepareBytes     = 64 << 10
)

type imageInspect struct {
	Configuration struct {
		Name       string `json:"name"`
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"descriptor"`
	} `json:"configuration"`
}

type ociImageConfig struct {
	Config struct {
		User   string            `json:"User"`
		Labels map[string]string `json:"Labels"`
	} `json:"config"`
	RootFS struct {
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// rootImageUser reports whether an OCI config user field keeps the
// runtime-default root user: unset, or an exact root user with an optional
// exact root group. Apple container's inspect report exposes no user field,
// so the runtime probe cannot observe a changed launch user; the digest-bound
// config is the only enforcement point (docs/plan.md §5.7).
func rootImageUser(user string) bool {
	name, group, hasGroup := strings.Cut(user, ":")
	if name != "" && name != "root" && name != "0" {
		return false
	}
	if !hasGroup {
		return true
	}
	return name != "" && (group == "root" || group == "0")
}

type appleBackend struct {
	containerPath    string
	runner           commandRunner
	registryLockDir  string
	inspectAllowlist func(context.Context, string) (ward.InspectReport, error)
	probeRegistry    func(context.Context, string) error
	waitRegistry     func(context.Context, string) error
	deleteManifest   func(context.Context, string) error
	readEvidence     func(context.Context, string, string, map[string]int64) (ociEvidence, error)
}

func (a appleBackend) ImageDigest(ctx context.Context, ref string) (string, error) {
	report, err := a.inspectImage(ctx, ref)
	if err != nil {
		return "", err
	}
	digest := report.Configuration.Descriptor.Digest
	if !validDigest(digest) {
		return "", fmt.Errorf("inspect image %q returned invalid manifest digest %q", ref, digest)
	}
	return digest, nil
}

func (a appleBackend) inspectImage(ctx context.Context, ref string) (imageInspect, error) {
	output, err := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"image", "inspect", ref},
	})
	if err != nil {
		return imageInspect{}, runError("inspect image "+ref, output, err)
	}
	var reports []imageInspect
	if err := decodeStrictJSON(output.bytes, &reports); err != nil {
		return imageInspect{}, fmt.Errorf("decode image inspection for %q: %w", ref, err)
	}
	if len(reports) != 1 {
		return imageInspect{}, fmt.Errorf("inspect image %q returned %d records, want exactly one", ref, len(reports))
	}
	if !sameInspectedImageName(reports[0].Configuration.Name, ref) {
		return imageInspect{}, fmt.Errorf("inspect image %q returned object %q", ref, reports[0].Configuration.Name)
	}
	return reports[0], nil
}

func sameInspectedImageName(observed, requested string) bool {
	if observed == requested {
		return true
	}
	return !strings.Contains(requested, "/") &&
		!strings.Contains(requested, "@") &&
		observed == "docker.io/library/"+requested
}

func (a appleBackend) PinBase(ctx context.Context, sourceRef, privateRef, digest string) (err error) {
	tagged := false
	defer func() {
		if tagged && err != nil {
			err = errors.Join(err, a.RemoveImage(context.WithoutCancel(ctx), privateRef))
		}
	}()
	output, err := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"image", "tag", sourceRef, privateRef},
	})
	if err != nil {
		return runError("pin private build base", output, err)
	}
	tagged = true
	observed, err := a.ImageDigest(ctx, privateRef)
	if err != nil {
		return err
	}
	if observed != digest {
		return fmt.Errorf("private build base resolves to %s, want %s", observed, digest)
	}
	return nil
}

func (a appleBackend) RemoveImage(ctx context.Context, ref string) error {
	output, err := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"image", "delete", ref},
	})
	return runError("remove image reference", output, err)
}

func (a appleBackend) Build(ctx context.Context, spec buildSpec) error {
	args := []string{"build"}
	for _, dns := range spec.DNS {
		args = append(args, "--dns", dns)
	}
	if spec.BuildProxy != "" {
		args = append(args,
			"--build-arg", "HTTP_PROXY="+spec.BuildProxy,
			"--build-arg", "HTTPS_PROXY="+spec.BuildProxy,
		)
	}
	args = append(args,
		"--build-arg", "BASE_IMAGE="+spec.BaseRef,
		"--build-arg", "BASE_DIGEST="+spec.BaseDigest,
		"--build-arg", "PROJECT_REPOSITORY="+spec.Repository,
		"--build-arg", "PROJECT_REPOSITORY_ID="+strconv.FormatInt(spec.RepositoryID, 10),
		"--build-arg", "PROJECT_COMMIT="+spec.CommitSHA,
		"--build-arg", "PROJECT_RECIPE_DIGEST="+string(spec.RecipeDigest),
		"--tag", spec.LocalRef,
		spec.ContextDir,
	)
	output, err := a.runner.Run(ctx, commandSpec{Path: a.containerPath, Args: args})
	return runError("container build", output, err)
}

func (a appleBackend) CheckProvenance(ctx context.Context, ref string, want provenanceSpec) error {
	if !validDigest(want.ImageDigest) {
		return fmt.Errorf("provenance check requires a valid built-image digest, got %q", want.ImageDigest)
	}
	report, err := a.inspectImage(ctx, ref)
	if err != nil {
		return err
	}
	if report.Configuration.Descriptor.Digest != want.ImageDigest {
		return fmt.Errorf("project image %q resolved to digest %s, want built digest %s",
			ref, report.Configuration.Descriptor.Digest, want.ImageDigest)
	}
	base, err := a.inspectImage(ctx, want.BaseBuildRef)
	if err != nil {
		return fmt.Errorf("reinspect build base: %w", err)
	}
	if base.Configuration.Descriptor.Digest != want.BaseDigest {
		return fmt.Errorf("build base changed to %s, want %s",
			base.Configuration.Descriptor.Digest, want.BaseDigest)
	}
	readEvidence := a.readEvidence
	if readEvidence == nil {
		readEvidence = a.readOCIEvidence
	}
	baseEvidence, err := readEvidence(ctx, want.BaseBuildRef, want.BaseDigest, map[string]int64{
		busyboxPath: maxBusyboxBytes,
	})
	if err != nil {
		return fmt.Errorf("host-verify build base: %w", err)
	}
	if user := baseEvidence.Config.Config.User; !rootImageUser(user) {
		return fmt.Errorf("build base config sets user %q, want the runtime-default root user", user)
	}
	projectEvidence, err := readEvidence(ctx, ref, want.ImageDigest, projectEvidenceTargets())
	if err != nil {
		return fmt.Errorf("host-verify project image: %w", err)
	}
	if user := projectEvidence.Config.Config.User; !rootImageUser(user) {
		return fmt.Errorf("project image config sets user %q, want the runtime-default root user", user)
	}
	if !bytes.Equal(baseEvidence.Files[busyboxPath], projectEvidence.Files[busyboxPath]) ||
		baseEvidence.FileModes[busyboxPath] != projectEvidence.FileModes[busyboxPath] {
		return fmt.Errorf("project image replaces the approved base's static BusyBox")
	}
	baseLayers := baseEvidence.Config.RootFS.DiffIDs
	projectLayers := projectEvidence.Config.RootFS.DiffIDs
	if len(baseLayers) == 0 || len(projectLayers) <= len(baseLayers) ||
		!slices.Equal(projectLayers[:len(baseLayers)], baseLayers) {
		return fmt.Errorf("project image rootfs is not derived from the approved base")
	}
	labels := projectEvidence.Config.Config.Labels
	expected := map[string]string{
		"org.opencontainers.image.title":                    "freeside-project-image",
		"ai.freeside.base.digest":                           want.BaseDigest,
		"ai.freeside.project.repository":                    want.Repository,
		"ai.freeside.project.repository-id":                 strconv.FormatInt(want.RepositoryID, 10),
		"ai.freeside.project.commit":                        want.CommitSHA,
		"ai.freeside.project.recipe-digest":                 string(want.RecipeDigest),
		"ai.freeside.project.toolchain.node.version":        want.NodeVersion,
		"ai.freeside.project.toolchain.node.archive-sha256": want.NodeToolchainArchiveSHA256,
	}
	for key, value := range expected {
		if labels[key] != value {
			return fmt.Errorf("image label %s = %q, want %q", key, labels[key], value)
		}
	}
	files := projectEvidence.Files
	modes := projectEvidence.FileModes
	recipeBytes, recipeFound := files[ward.ProjectRecipePath]
	prepareBytes, prepareFound := files[PreparationPath]
	if !recipeFound || !prepareFound {
		return fmt.Errorf("exported rootfs omitted embedded provenance files")
	}
	recipeDigest := contentaddr.Sum(recipeBytes)
	if recipeDigest != string(want.RecipeDigest) ||
		!bytes.Equal(prepareBytes, []byte(prepareScript)) {
		return fmt.Errorf("embedded provenance file digests do not match the build inputs")
	}
	archive, archiveFound := files[nodeToolchainArchivePath]
	archiveDigest := fmt.Sprintf("%x", sha256.Sum256(archive))
	if !archiveFound || archiveDigest != want.NodeToolchainArchiveSHA256 {
		return fmt.Errorf("embedded Node toolchain archive does not match the pinned input")
	}
	if modes[nodeToolchainArchivePath] != 0o644 || modes[PreparationPath] != 0o700 {
		return fmt.Errorf("embedded project-image provenance files have invalid modes")
	}
	for _, launcher := range []string{nodeLauncherPath, npmLauncherPath, npxLauncherPath} {
		if !bytes.Equal(files[launcher], []byte(nodeToolchainLauncher)) || modes[launcher] != 0o755 {
			return fmt.Errorf("embedded Node toolchain launcher %s does not match the builder input", launcher)
		}
	}
	return nil
}

func (a appleBackend) CheckAllowlist(ctx context.Context, ref string) (err error) {
	token, err := randomToken()
	if err != nil {
		return err
	}
	cidPath, err := newCIDPath()
	if err != nil {
		return err
	}
	defer func() {
		removeErr := os.Remove(cidPath)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove agent-image allowlist identity file: %w", removeErr))
		}
	}()
	output, runErr := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath,
		Args: []string{
			"create", "--cidfile", cidPath,
			"--label", ownershipLabel + "=" + token,
			"--", ref, "sh", "-c", "true",
		},
	})
	containerID, identityErr := readContainerID(cidPath)
	var recoverErr error
	if identityErr == nil {
		defer func() {
			cleanupErr := a.deleteOwnedContainer(
				context.WithoutCancel(ctx), containerID, token,
			)
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("clean agent-image allowlist probe: %w", cleanupErr))
			}
		}()
	} else {
		recoverErr = a.recoverOwnedContainer(context.WithoutCancel(ctx), token)
		if runErr == nil {
			return errors.Join(identityErr, recoverErr)
		}
	}
	if runErr != nil {
		return errors.Join(runError("create agent-image allowlist probe", output, runErr), recoverErr)
	}
	inspect := a.inspectAllowlist
	if inspect == nil {
		inspect = ward.NewCLIRuntime(a.containerPath).Inspect
	}
	report, err := inspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspect agent-image allowlist probe: %w", err)
	}
	return validateImageAllowlist(report, containerID, ref)
}

func validateImageAllowlist(
	report ward.InspectReport,
	containerID string,
	imageRef string,
) error {
	switch {
	case report.ID != containerID:
		return errors.New("agent-image allowlist inspection identified the wrong container")
	case !report.AllowlistFieldsObserved:
		return errors.New("agent-image allowlist inspection omitted required configuration")
	case report.State != ward.StateStopped:
		return errors.New("agent-image allowlist probe was not observed stopped")
	case !sameInspectedImageName(report.ImageReference, imageRef):
		return errors.New("agent-image allowlist inspection reported the wrong image")
	case len(report.Env) != 1 || report.Env[0] != allowlistPathEnv:
		return errors.New("agent image contributes environment beyond the fixed PATH")
	case report.WorkingDirectory != "/":
		return errors.New("agent image changes the fixed root working directory")
	case !slices.Equal(report.Command, []string{"sh", "-c", "true"}):
		return errors.New("agent image changes the supplied allowlist command")
	case len(report.Mounts) != 0:
		return errors.New("agent image declares a mount")
	case report.SSH:
		return errors.New("agent image enables SSH forwarding")
	case len(report.PublishedPorts) != 0 || len(report.PublishedSockets) != 0:
		return errors.New("agent image declares a host publication")
	case !report.NetworksObserved:
		return errors.New("agent-image allowlist inspection omitted network attachments")
	default:
		return nil
	}
}

func (a appleBackend) Run(ctx context.Context, spec runSpec) (result runResult, err error) {
	token, err := randomToken()
	if err != nil {
		return runResult{}, err
	}
	cidPath, err := newCIDPath()
	if err != nil {
		return runResult{}, err
	}
	defer os.Remove(cidPath) //nolint:errcheck // private one-shot runtime identity
	args := []string{
		"run", "--rm", "--cidfile", cidPath,
		"--label", ownershipLabel + "=" + token,
		"--network", "none",
	}
	if spec.MaskCache {
		args = append(args, "--tmpfs", NPMCachePath)
	}
	if spec.Workspace != "" {
		args = append(args,
			"--volume", spec.Workspace+":/workspace",
			"--workdir", "/workspace",
		)
	}
	args = append(args, "--", spec.ImageRef)
	args = append(args, spec.Argv...)
	output, runErr := a.runner.Run(ctx, commandSpec{Path: a.containerPath, Args: args})
	containerID, identityErr := readContainerID(cidPath)
	var recoverErr error
	if identityErr == nil {
		defer func() {
			cleanupErr := a.deleteOwnedContainer(
				context.WithoutCancel(ctx), containerID, token,
			)
			if cleanupErr != nil {
				result = runResult{}
				err = errors.Join(err, cleanupErr)
			}
		}()
	} else {
		recoverErr = a.recoverOwnedContainer(context.WithoutCancel(ctx), token)
		if runErr == nil {
			return runResult{}, errors.Join(identityErr, recoverErr)
		}
	}
	if runErr != nil {
		if ctx.Err() != nil {
			if recoverErr != nil {
				return runResult{}, recoverErr
			}
			return runResult{
				Output:   append([]byte{}, output.bytes...),
				ExitCode: -1, Truncated: output.truncated,
			}, nil
		}
		if identityErr != nil || !output.exited {
			return runResult{}, errors.Join(
				runError("run project-image command", output, runErr), recoverErr)
		}
		exitCode := output.exitCode
		if exitCode < 0 || (exitCode >= 128 && exitCode <= 192) {
			exitCode = -1
		}
		return runResult{
			Output:   append([]byte{}, output.bytes...),
			ExitCode: exitCode, Truncated: output.truncated,
		}, nil
	}
	if !output.exited || output.exitCode != 0 {
		return runResult{}, fmt.Errorf(
			"project-image runtime returned inconsistent successful status")
	}
	return runResult{
		Output:   append([]byte{}, output.bytes...),
		ExitCode: 0, Truncated: output.truncated,
	}, nil
}

func (a appleBackend) Publish(
	ctx context.Context,
	spec publishSpec,
) (published publication, err error) {
	registry := strings.TrimSuffix(spec.Registry, "/")
	scheme := "auto"
	registryContainer := ""
	registryToken := ""
	reusedRegistry := false
	var releaseRegistryLease func() error
	cleanup := func() error {
		if registryContainer == "" {
			return nil
		}
		if removeErr := a.deleteOwnedContainer(
			context.WithoutCancel(ctx), registryContainer, registryToken,
		); removeErr != nil {
			return fmt.Errorf("remove temporary registry: %w", removeErr)
		}
		registryContainer = ""
		registryToken = ""
		return nil
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
			published = publication{}
		}
		if releaseRegistryLease != nil {
			if releaseErr := releaseRegistryLease(); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("release local registry lease: %w", releaseErr))
				published = publication{}
			}
		}
	}()

	if spec.LocalRegistryPort != 0 {
		lockDir := a.registryLockDir
		if lockDir == "" {
			cacheDir, cacheErr := os.UserCacheDir()
			if cacheErr != nil {
				return publication{}, fmt.Errorf("resolve local registry lease directory: %w", cacheErr)
			}
			lockDir = filepath.Join(cacheDir, "freeside", "project-image-registry-locks")
		}
		if mkdirErr := os.MkdirAll(lockDir, 0o700); mkdirErr != nil {
			return publication{}, fmt.Errorf("create local registry lease directory: %w", mkdirErr)
		}
		releaseRegistryLease, err = acquireLocalRegistryLease(
			ctx, lockDir, spec.LocalRegistryPort,
		)
		if err != nil {
			return publication{}, err
		}
		registry = "127.0.0.1:" + strconv.Itoa(spec.LocalRegistryPort)
		scheme = "http"
		registryURL := "http://" + registry + "/v2/"
		probe := a.probeRegistry
		if probe == nil {
			probe = probeRegistry
		}
		var retained bool
		var lookupErr error
		registryContainer, registryToken, retained, lookupErr = a.findRetainedRegistry(
			ctx, spec.LocalRegistryPort,
		)
		if lookupErr != nil {
			return publication{}, lookupErr
		}
		probeErr := probe(ctx, registryURL)
		if retained {
			if probeErr != nil {
				wait := a.waitRegistry
				if wait == nil {
					wait = waitForRegistry
				}
				if waitErr := wait(ctx, registryURL); waitErr != nil {
					return publication{}, fmt.Errorf(
						"retained project-image registry on %s is not ready: %w",
						registry, errors.Join(probeErr, waitErr))
				}
			}
			// This invocation verified but did not create the retained
			// service, so it must not inherit cleanup ownership.
			registryContainer = ""
			registryToken = ""
			reusedRegistry = true
		} else if probeErr == nil {
			return publication{}, fmt.Errorf(
				"loopback registry port %d is occupied by an unmanaged service",
				spec.LocalRegistryPort)
		} else {
			if ctx.Err() != nil {
				return publication{}, ctx.Err()
			}
			if err := a.ensureRegistryImage(ctx); err != nil {
				return publication{}, err
			}
			token, err := randomToken()
			if err != nil {
				return publication{}, err
			}
			cidPath, err := newCIDPath()
			if err != nil {
				return publication{}, err
			}
			defer os.Remove(cidPath) //nolint:errcheck // private one-shot runtime identity
			output, runErr := a.runner.Run(ctx, commandSpec{
				Path: a.containerPath,
				Args: []string{
					"run", "--detach", "--cidfile", cidPath,
					"--label", ownershipLabel + "=" + token,
					"--label", registryPortLabel + "=" +
						strconv.Itoa(spec.LocalRegistryPort),
					// Manifest deletion must be enabled at creation so a later
					// build reusing this retained registry can remove the
					// residue of its own failed publication (#352).
					"--env", "REGISTRY_STORAGE_DELETE_ENABLED=true",
					"--publish", registry + ":5000", registryImage,
				},
			})
			containerID, identityErr := readContainerID(cidPath)
			var recoverErr error
			if identityErr == nil {
				registryContainer = containerID
				registryToken = token
			} else {
				recoverErr = a.recoverOwnedContainer(context.WithoutCancel(ctx), token)
				if runErr == nil {
					return publication{}, errors.Join(identityErr, recoverErr)
				}
			}
			if runErr != nil {
				return publication{}, errors.Join(
					runError("start temporary registry", output, runErr), recoverErr)
			}
			wait := a.waitRegistry
			if wait == nil {
				wait = waitForRegistry
			}
			if err := wait(ctx, registryURL); err != nil {
				return publication{}, err
			}
		}
	}

	tagToken, err := randomToken()
	if err != nil {
		return publication{}, fmt.Errorf("generate publication tag: %w", err)
	}
	tagRef := registry + "/" + spec.ImageName + ":" +
		temporaryPublicationTag(spec.RefTag, tagToken)
	digestRef := domain.ImageRef(registry + "/" + spec.ImageName + "@" + spec.Digest)
	manifestPushAttempted := false
	seededLocally := false
	discardTransferred := false
	discardResidue := func(discardCtx context.Context) error {
		return a.discardPublicationResidue(discardCtx, spec, registry,
			string(digestRef), seededLocally,
			manifestPushAttempted && reusedRegistry && spec.LocalRegistryPort != 0)
	}
	// Registered before the tag defer so LIFO order discards residue while
	// the same-port lease is still held: the lease is what keeps the
	// recorded-reference guard's answer valid against a concurrent build.
	// The transfer flag, not the error, gates the discard so a panic after
	// a residue exists still removes it.
	defer func() {
		if discardTransferred {
			return
		}
		if discardErr := discardResidue(context.WithoutCancel(ctx)); discardErr != nil {
			err = errors.Join(err, discardErr)
			published = publication{}
		}
	}()
	output, runErr := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"image", "tag", spec.LocalRef, tagRef},
	})
	if runErr != nil {
		return publication{}, runError("tag project image", output, runErr)
	}
	tagged := true
	defer func() {
		if !tagged {
			return
		}
		if removeErr := a.RemoveImage(context.WithoutCancel(ctx), tagRef); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove publication tag: %w", removeErr))
			published = publication{}
		}
	}()
	// The one-shot tag this push creates stays behind in a retained registry
	// after a successful build: registry:2 cannot delete a tag without
	// deleting its manifest, and on success that manifest is the durably
	// recorded artifact. That bounded retention (one tag per successful
	// publication) is an accepted decision (#352). A failed build's manifest
	// deletion removes its tags with the manifest, except when the
	// recorded-reference guard retains the manifest: its tag link then stays
	// too, an accepted corner recorded in the decision note.
	//
	// Armed before the attempt: a push whose connection dies after the
	// registry commits the manifest still reports failure, so an attempted
	// push must count as potentially committed. The 404 disposition makes
	// deleting a never-committed manifest a no-op.
	manifestPushAttempted = true
	output, runErr = a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"image", "push", "--scheme", scheme, tagRef},
	})
	if runErr != nil {
		return publication{}, runError("push project image", output, runErr)
	}
	output, runErr = a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"image", "pull", "--scheme", scheme, string(digestRef)},
	})
	if runErr != nil {
		return publication{}, runError("seed exact project-image reference", output, runErr)
	}
	seededLocally = true
	seeded, digestErr := a.ImageDigest(ctx, string(digestRef))
	if digestErr != nil {
		return publication{}, digestErr
	}
	if seeded != spec.Digest {
		return publication{}, fmt.Errorf(
			"seeded digest %s does not match built digest %s", seeded, spec.Digest)
	}
	if err := a.RemoveImage(context.WithoutCancel(ctx), tagRef); err != nil {
		return publication{}, fmt.Errorf("remove publication tag: %w", err)
	}
	tagged = false
	published.Ref = digestRef
	if registryContainer != "" {
		name, token := registryContainer, registryToken
		published.cleanup = func(cleanupCtx context.Context) error {
			return a.deleteOwnedContainer(cleanupCtx, name, token)
		}
		// Transfer cleanup ownership to the returned publication lease. The
		// builder retains it only after its recorder succeeds.
		registryContainer = ""
		registryToken = ""
	}
	// Both residue flags are true here, so the transferred discard covers
	// the builder's post-publication failures (validate, allowlist, record)
	// until the recorder disarms it.
	published.discard = discardResidue
	discardTransferred = true
	published.release = releaseRegistryLease
	releaseRegistryLease = nil
	return published, nil
}

// discardPublicationResidue removes what a failed publication left behind:
// the locally seeded exact-digest image and, for a reused loopback registry
// this invocation does not own (an owned registry is deleted whole by its
// cleanup), the pushed manifest. The recorded-reference guard fails toward
// retention: a rebuild that reproduces an already-recorded digest must not
// destroy the prior row's durable artifact (#352).
func (a appleBackend) discardPublicationResidue(
	ctx context.Context,
	spec publishSpec,
	registry, digestRef string,
	removeSeeded, dropManifest bool,
) error {
	if !removeSeeded && !dropManifest {
		return nil
	}
	if spec.RefRecorded != nil {
		recorded, lookupErr := spec.RefRecorded(ctx, digestRef)
		if lookupErr != nil {
			return fmt.Errorf("check recorded project-image reference before discard: %w", lookupErr)
		}
		if recorded {
			return nil
		}
	}
	var errs []error
	if removeSeeded {
		if removeErr := a.RemoveImage(ctx, digestRef); removeErr != nil {
			errs = append(errs, fmt.Errorf("remove seeded project-image reference: %w", removeErr))
		}
	}
	if dropManifest {
		remove := a.deleteManifest
		if remove == nil {
			remove = deleteRegistryManifest
		}
		url := "http://" + registry + "/v2/" + spec.ImageName + "/manifests/" + spec.Digest
		if deleteErr := remove(ctx, url); deleteErr != nil {
			errs = append(errs, fmt.Errorf("delete published project-image manifest: %w", deleteErr))
		}
	}
	return errors.Join(errs...)
}

func acquireLocalRegistryLease(
	ctx context.Context,
	lockDir string,
	port int,
) (func() error, error) {
	lockPath := filepath.Join(lockDir, strconv.Itoa(port)+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // fixed numeric port under the owner-private cache directory
	if err != nil {
		return nil, fmt.Errorf("open local registry lease for port %d: %w", port, err)
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() error {
				return errors.Join(unix.Flock(int(lock.Fd()), unix.LOCK_UN), lock.Close())
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = lock.Close()
			return nil, fmt.Errorf("lock local registry port %d: %w", port, err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func temporaryPublicationTag(prefix, token string) string {
	const maxTagLength = 128
	maxPrefixLength := maxTagLength - 1 - len(token)
	if len(prefix) > maxPrefixLength {
		prefix = prefix[:maxPrefixLength]
	}
	return prefix + "-" + token
}

func containerInspectionAbsent(output commandOutput) bool {
	return strings.Contains(strings.ToLower(string(output.bytes)), "container not found:")
}

func containerDeletionAbsent(output commandOutput, id string) bool {
	text := strings.ToLower(string(output.bytes))
	return strings.Contains(text, "notfound:") &&
		strings.Contains(text, "container with id "+strings.ToLower(id)+" not found")
}

type containerInspect struct {
	ID            string `json:"id"`
	Configuration struct {
		ID     string            `json:"id"`
		Labels map[string]string `json:"labels"`
		Image  struct {
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"image"`
	} `json:"configuration"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func randomToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func newCIDPath() (string, error) {
	file, err := os.CreateTemp("", "freeside-container-id-*")
	if err != nil {
		return "", fmt.Errorf("create container identity path: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close container identity path: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("prepare container identity path: %w", err)
	}
	return path, nil
}

func readContainerID(path string) (string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // private path from newCIDPath
	if err != nil {
		return "", fmt.Errorf("read runtime-assigned container ID: %w", err)
	}
	id := strings.TrimSpace(string(body))
	if !validContainerID(id) {
		return "", fmt.Errorf("runtime returned invalid container ID %q", id)
	}
	return id, nil
}

func validContainerID(id string) bool {
	return len(id) > 0 && len(id) <= 128 &&
		((id[0] >= 'a' && id[0] <= 'z') ||
			(id[0] >= 'A' && id[0] <= 'Z') ||
			(id[0] >= '0' && id[0] <= '9')) &&
		strings.IndexFunc(id, func(r rune) bool {
			return (r < 'a' || r > 'z') &&
				(r < 'A' || r > 'Z') &&
				(r < '0' || r > '9') &&
				r != '-' && r != '_' && r != '.'
		}) == -1
}

func (a appleBackend) inspectContainer(
	ctx context.Context,
	name string,
) (containerInspect, bool, error) {
	output, err := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"inspect", name},
	})
	if err != nil {
		if containerInspectionAbsent(output) {
			return containerInspect{}, false, nil
		}
		return containerInspect{}, false, runError("inspect temporary container "+name, output, err)
	}
	var reports []containerInspect
	if err := decodeStrictJSON(output.bytes, &reports); err != nil {
		return containerInspect{}, false, fmt.Errorf(
			"decode temporary container inspection for %q: %w", name, err)
	}
	if len(reports) != 1 || reports[0].ID != name || reports[0].Configuration.ID != name {
		return containerInspect{}, false, fmt.Errorf(
			"inspect temporary container %q returned an unexpected object", name)
	}
	return reports[0], true, nil
}

func (a appleBackend) deleteOwnedContainer(ctx context.Context, id, token string) error {
	report, exists, err := a.inspectContainer(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if report.Configuration.Labels[ownershipLabel] != token {
		return fmt.Errorf("refuse to delete unowned temporary container %q", id)
	}
	output, err := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"delete", "--force", report.ID},
	})
	if err != nil {
		// `container run --rm` removes a successful process asynchronously.
		// It can disappear after the ownership inspection but before this
		// defensive delete; exact-ID absence is the intended end state.
		if containerDeletionAbsent(output, report.ID) {
			return nil
		}
		return runError("delete owned temporary container "+id, output, err)
	}
	return nil
}

// recoverOwnedContainer deletes any container carrying this invocation's
// one-shot ownership token after the runtime-generated identity file was
// lost: the token is the only remaining binding to an instance an
// interrupted create may have left behind. Apple container's list output
// omits labels, so ownership is read per candidate from its inspection
// (the findRetainedRegistry pattern), and deletion still re-gates through
// deleteOwnedContainer's own inspection.
func (a appleBackend) recoverOwnedContainer(ctx context.Context, token string) error {
	output, runErr := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"list", "--all", "--format", "json"},
	})
	if runErr != nil {
		return runError("list containers for ownership recovery", output, runErr)
	}
	var reports []containerInspect
	if err := decodeStrictJSON(output.bytes, &reports); err != nil {
		return fmt.Errorf("decode containers for ownership recovery: %w", err)
	}
	var recovered error
	for _, listed := range reports {
		if !validContainerID(listed.ID) || listed.Configuration.ID != listed.ID {
			recovered = errors.Join(recovered, fmt.Errorf(
				"ownership recovery listed invalid container identity %q", listed.ID))
			continue
		}
		report, exists, err := a.inspectContainer(ctx, listed.ID)
		if err != nil {
			recovered = errors.Join(recovered, err)
			continue
		}
		if !exists || report.Configuration.Labels[ownershipLabel] != token {
			continue
		}
		recovered = errors.Join(recovered, a.deleteOwnedContainer(ctx, listed.ID, token))
	}
	return recovered
}

func (a appleBackend) findRetainedRegistry(
	ctx context.Context,
	port int,
) (id string, token string, found bool, err error) {
	output, runErr := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"list", "--all", "--format", "json"},
	})
	if runErr != nil {
		return "", "", false, runError("list retained project-image registries", output, runErr)
	}
	var reports []containerInspect
	if err := decodeStrictJSON(output.bytes, &reports); err != nil {
		return "", "", false, fmt.Errorf("decode retained project-image registries: %w", err)
	}
	portLabel := strconv.Itoa(port)
	var candidate containerInspect
	for _, listed := range reports {
		if !validContainerID(listed.ID) || listed.Configuration.ID != listed.ID {
			return "", "", false, fmt.Errorf(
				"container list returned an unexpected object")
		}
		report, exists, err := a.inspectContainer(ctx, listed.ID)
		if err != nil {
			return "", "", false, err
		}
		if !exists || report.Configuration.Labels[registryPortLabel] != portLabel {
			continue
		}
		if found {
			return "", "", false, fmt.Errorf(
				"multiple retained project-image registries claim loopback port %d", port)
		}
		candidate = report
		found = true
	}
	if !found {
		return "", "", false, nil
	}
	token = candidate.Configuration.Labels[ownershipLabel]
	if token == "" || candidate.ID == "" ||
		candidate.Configuration.ID != candidate.ID ||
		candidate.Configuration.Image.Descriptor.Digest != registryImageDigest ||
		(candidate.Status.State != "running" && candidate.Status.State != "stopped") {
		return "", "", false, fmt.Errorf(
			"retained project-image registry on loopback port %d has invalid ownership",
			port)
	}
	state := candidate.Status.State
	inspected, exists, err := a.inspectContainer(ctx, candidate.ID)
	if err != nil {
		return "", "", false, err
	}
	if !exists ||
		inspected.Configuration.Labels[ownershipLabel] != token ||
		inspected.Configuration.Labels[registryPortLabel] != portLabel ||
		inspected.Configuration.Image.Descriptor.Digest != registryImageDigest ||
		inspected.Status.State != state {
		return "", "", false, fmt.Errorf(
			"retained project-image registry on loopback port %d changed during inspection",
			port)
	}
	if state == "stopped" {
		output, startErr := a.runner.Run(ctx, commandSpec{
			Path: a.containerPath, Args: []string{"start", inspected.ID},
		})
		if startErr != nil {
			return "", "", false, fmt.Errorf(
				"restart retained project-image registry: %w",
				runError("start retained project-image registry", output, startErr))
		}
		restarted, exists, err := a.inspectContainer(ctx, inspected.ID)
		if err != nil {
			return "", "", false, err
		}
		if !exists ||
			restarted.Configuration.Labels[ownershipLabel] != token ||
			restarted.Configuration.Labels[registryPortLabel] != portLabel ||
			restarted.Configuration.Image.Descriptor.Digest != registryImageDigest ||
			restarted.Status.State != "running" {
			return "", "", false, fmt.Errorf(
				"retained project-image registry on loopback port %d did not restart safely",
				port)
		}
		return restarted.ID, token, true, nil
	}
	return inspected.ID, token, true, nil
}

func (a appleBackend) readOCIEvidence(
	ctx context.Context,
	ref string,
	expectedDigest string,
	wanted map[string]int64,
) (ociEvidence, error) {
	archive, err := os.CreateTemp("", "freeside-project-rootfs-*.tar")
	if err != nil {
		return ociEvidence{}, fmt.Errorf("create provenance export path: %w", err)
	}
	archivePath := archive.Name()
	if err := archive.Close(); err != nil {
		return ociEvidence{}, fmt.Errorf("close provenance export path: %w", err)
	}
	if err := os.Remove(archivePath); err != nil {
		return ociEvidence{}, fmt.Errorf("prepare provenance export path: %w", err)
	}
	defer os.Remove(archivePath) //nolint:errcheck // private one-shot proof artifact
	output, err := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath,
		Args: []string{"image", "save", "--output", archivePath, ref},
	})
	if err != nil {
		return ociEvidence{}, runError("save image as OCI archive", output, err)
	}
	return readOCIEvidence(archivePath, expectedDigest, wanted)
}

type ociDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

type ociEvidence struct {
	Config    ociImageConfig
	Files     map[string][]byte
	FileModes map[string]int64
}

const (
	ociIndexMediaType    = "application/vnd.oci.image.index.v1+json"
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	ociConfigMediaType   = "application/vnd.oci.image.config.v1+json"
	ociLayerMediaType    = "application/vnd.oci.image.layer.v1.tar"
	ociGzipMediaType     = "application/vnd.oci.image.layer.v1.tar+gzip"
	maxOCIJSONBytes      = 4 << 20
	maxOCILayers         = 128
	maxOCILayerBytes     = 1 << 30
	maxNodeArchiveBytes  = 64 << 20
	maxBusyboxBytes      = 8 << 20
)

func projectEvidenceTargets() map[string]int64 {
	return map[string]int64{
		ward.ProjectRecipePath:   maxRecipeBytes,
		PreparationPath:          maxPrepareBytes,
		nodeToolchainArchivePath: maxNodeArchiveBytes,
		nodeLauncherPath:         maxPrepareBytes,
		npmLauncherPath:          maxPrepareBytes,
		npxLauncherPath:          maxPrepareBytes,
		busyboxPath:              maxBusyboxBytes,
	}
}

func readOCIEvidence(
	archivePath string,
	expectedDigest string,
	wanted map[string]int64,
) (ociEvidence, error) {
	indexBytes, err := readOCIArchiveEntry(archivePath, "index.json", maxOCIJSONBytes)
	if err != nil {
		return ociEvidence{}, err
	}
	var index ociIndex
	if err := decodeStrictJSON(indexBytes, &index); err != nil {
		return ociEvidence{}, fmt.Errorf("decode OCI image index: %w", err)
	}
	if index.SchemaVersion != 2 || index.MediaType != ociIndexMediaType ||
		len(index.Manifests) != 1 || index.Manifests[0].Digest != expectedDigest {
		return ociEvidence{}, fmt.Errorf(
			"OCI image index does not bind exact digest %s", expectedDigest)
	}
	descriptor := index.Manifests[0]
	payload, err := readOCIBlob(archivePath, descriptor, maxOCIJSONBytes)
	if err != nil {
		return ociEvidence{}, err
	}
	if descriptor.MediaType == ociIndexMediaType {
		var platformIndex ociIndex
		if err := decodeStrictJSON(payload, &platformIndex); err != nil {
			return ociEvidence{}, fmt.Errorf("decode OCI platform index: %w", err)
		}
		if platformIndex.SchemaVersion != 2 ||
			platformIndex.MediaType != ociIndexMediaType ||
			len(platformIndex.Manifests) != 1 {
			return ociEvidence{}, fmt.Errorf("OCI platform index is not a single proven image")
		}
		descriptor = platformIndex.Manifests[0]
		payload, err = readOCIBlob(archivePath, descriptor, maxOCIJSONBytes)
		if err != nil {
			return ociEvidence{}, err
		}
	}
	if descriptor.MediaType != ociManifestMediaType {
		return ociEvidence{}, fmt.Errorf(
			"OCI image resolved to unsupported manifest type %q", descriptor.MediaType)
	}
	var manifest ociManifest
	if err := decodeStrictJSON(payload, &manifest); err != nil {
		return ociEvidence{}, fmt.Errorf("decode OCI image manifest: %w", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != ociManifestMediaType ||
		len(manifest.Layers) == 0 || len(manifest.Layers) > maxOCILayers {
		return ociEvidence{}, fmt.Errorf("OCI image manifest has invalid layer set")
	}
	config, err := readOCIConfig(archivePath, manifest)
	if err != nil {
		return ociEvidence{}, err
	}
	layerDir, err := os.MkdirTemp("", "freeside-project-layers-*")
	if err != nil {
		return ociEvidence{}, fmt.Errorf("create OCI layer scratch: %w", err)
	}
	defer os.RemoveAll(layerDir) //nolint:errcheck // private one-shot proof artifact
	layerPaths, err := extractOCILayers(archivePath, manifest.Layers, layerDir)
	if err != nil {
		return ociEvidence{}, err
	}
	if err := verifyOCIDiffIDs(layerPaths, manifest.Layers, config.RootFS.DiffIDs); err != nil {
		return ociEvidence{}, err
	}
	evidence := ociEvidence{Config: config}
	if len(wanted) == 0 {
		return evidence, nil
	}
	files := make(map[string][]byte, len(wanted))
	modes := make(map[string]int64, len(wanted))
	for index := len(manifest.Layers) - 1; index >= 0; index-- {
		if err := readProvenanceLayer(
			layerPaths[index], manifest.Layers[index].MediaType, wanted, files, modes,
		); err != nil {
			return ociEvidence{}, err
		}
		if len(files) == len(wanted) {
			break
		}
	}
	for name := range wanted {
		if _, ok := files[name]; !ok {
			return ociEvidence{}, fmt.Errorf("project image rootfs lacks %s", name)
		}
	}
	evidence.Files = files
	evidence.FileModes = modes
	return evidence, nil
}

func readOCIConfig(archivePath string, manifest ociManifest) (ociImageConfig, error) {
	if manifest.Config.MediaType != ociConfigMediaType {
		return ociImageConfig{}, fmt.Errorf(
			"OCI image config has unsupported media type %q", manifest.Config.MediaType)
	}
	body, err := readOCIBlob(archivePath, manifest.Config, maxOCIJSONBytes)
	if err != nil {
		return ociImageConfig{}, err
	}
	var config ociImageConfig
	if err := decodeStrictJSON(body, &config); err != nil {
		return ociImageConfig{}, fmt.Errorf("decode OCI image config: %w", err)
	}
	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return ociImageConfig{}, fmt.Errorf(
			"OCI image config has %d diff IDs for %d layers",
			len(config.RootFS.DiffIDs), len(manifest.Layers))
	}
	for _, digest := range config.RootFS.DiffIDs {
		if !validDigest(digest) {
			return ociImageConfig{}, fmt.Errorf(
				"OCI image config has invalid diff ID %q", digest)
		}
	}
	return config, nil
}

func decodeStrictJSON(body []byte, destination any) error {
	if err := ward.RejectDuplicateJSONKeys(body); err != nil {
		return err
	}
	return json.Unmarshal(body, destination)
}

func verifyOCIDiffIDs(
	layerPaths []string,
	layers []ociDescriptor,
	diffIDs []string,
) error {
	if len(layerPaths) != len(layers) || len(layers) != len(diffIDs) {
		return fmt.Errorf("OCI layer and diff ID counts do not match")
	}
	for index, layerPath := range layerPaths {
		layer, err := os.Open(layerPath) //nolint:gosec // private verified layer path
		if err != nil {
			return err
		}
		var reader io.Reader = layer
		var compressed *gzip.Reader
		if layers[index].MediaType == ociGzipMediaType {
			compressed, err = gzip.NewReader(layer)
			if err != nil {
				_ = layer.Close()
				return fmt.Errorf("decompress OCI layer %d: %w", index, err)
			}
			reader = compressed
		}
		hash := sha256.New()
		written, copyErr := io.Copy(hash, io.LimitReader(reader, maxOCILayerBytes+1))
		if compressed != nil {
			copyErr = errors.Join(copyErr, compressed.Close())
		}
		closeErr := layer.Close()
		if copyErr != nil {
			return fmt.Errorf("hash OCI layer %d: %w", index, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		observed := contentaddr.Format(hash.Sum(nil))
		if written > maxOCILayerBytes || observed != diffIDs[index] {
			return fmt.Errorf(
				"OCI layer %d does not match config diff ID %s", index, diffIDs[index])
		}
	}
	return nil
}

func readOCIArchiveEntry(archivePath, entryName string, limit int64) ([]byte, error) {
	archive, err := os.Open(archivePath) //nolint:gosec // private runtime export
	if err != nil {
		return nil, err
	}
	defer archive.Close() //nolint:errcheck // read-only runtime export
	reader := tar.NewReader(archive)
	var result []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read OCI archive: %w", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name != entryName {
			continue
		}
		if result != nil || header.Typeflag != tar.TypeReg ||
			header.Size < 0 || header.Size > limit {
			return nil, fmt.Errorf("OCI archive has invalid %s", entryName)
		}
		body, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil {
			return nil, fmt.Errorf("read %s from OCI archive: %w", entryName, err)
		}
		if int64(len(body)) != header.Size {
			return nil, fmt.Errorf("OCI archive truncated %s", entryName)
		}
		result = body
	}
	if result == nil {
		return nil, fmt.Errorf("OCI archive lacks %s", entryName)
	}
	return result, nil
}

func readOCIBlob(archivePath string, descriptor ociDescriptor, limit int64) ([]byte, error) {
	if !validDigest(descriptor.Digest) || descriptor.Size < 0 || descriptor.Size > limit {
		return nil, fmt.Errorf("OCI descriptor is invalid")
	}
	entryName := "blobs/sha256/" + contentaddr.Hex(descriptor.Digest)
	body, err := readOCIArchiveEntry(archivePath, entryName, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != descriptor.Size ||
		contentaddr.Sum(body) != descriptor.Digest {
		return nil, fmt.Errorf("OCI blob %s does not match its descriptor", descriptor.Digest)
	}
	return body, nil
}

func extractOCILayers(
	archivePath string,
	layers []ociDescriptor,
	destination string,
) ([]string, error) {
	byEntry := make(map[string]int, len(layers))
	paths := make([]string, len(layers))
	for index, descriptor := range layers {
		if !validDigest(descriptor.Digest) || descriptor.Size < 0 ||
			descriptor.Size > maxOCILayerBytes ||
			(descriptor.MediaType != ociLayerMediaType &&
				descriptor.MediaType != ociGzipMediaType) {
			return nil, fmt.Errorf("OCI layer descriptor %d is invalid", index)
		}
		entry := "blobs/sha256/" + contentaddr.Hex(descriptor.Digest)
		if _, duplicate := byEntry[entry]; duplicate {
			return nil, fmt.Errorf("OCI image repeats layer %s", descriptor.Digest)
		}
		byEntry[entry] = index
		paths[index] = filepath.Join(destination, strconv.Itoa(index)+".layer")
	}
	archive, err := os.Open(archivePath) //nolint:gosec // private runtime export
	if err != nil {
		return nil, err
	}
	defer archive.Close() //nolint:errcheck // read-only runtime export
	found := make([]bool, len(layers))
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read OCI archive layers: %w", err)
		}
		entry := strings.TrimPrefix(header.Name, "./")
		index, ok := byEntry[entry]
		if !ok {
			continue
		}
		descriptor := layers[index]
		if found[index] || header.Typeflag != tar.TypeReg || header.Size != descriptor.Size {
			return nil, fmt.Errorf("OCI archive has invalid layer %s", descriptor.Digest)
		}
		layer, err := os.OpenFile(paths[index], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(layer, hash), reader, descriptor.Size)
		closeErr := layer.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if written != descriptor.Size ||
			contentaddr.Format(hash.Sum(nil)) != descriptor.Digest {
			return nil, fmt.Errorf("OCI layer %s does not match its descriptor", descriptor.Digest)
		}
		found[index] = true
	}
	for index, ok := range found {
		if !ok {
			return nil, fmt.Errorf("OCI archive lacks layer %s", layers[index].Digest)
		}
	}
	return paths, nil
}

func readProvenanceLayer(
	layerPath, mediaType string,
	wanted map[string]int64,
	files map[string][]byte,
	modes map[string]int64,
) error {
	layer, err := os.Open(layerPath) //nolint:gosec // fixed index under private layer scratch
	if err != nil {
		return err
	}
	defer layer.Close() //nolint:errcheck // read-only verified OCI layer
	var source io.Reader = layer
	if mediaType == ociGzipMediaType {
		compressed, err := gzip.NewReader(layer)
		if err != nil {
			return fmt.Errorf("decompress OCI layer: %w", err)
		}
		defer compressed.Close() //nolint:errcheck // read-only verified OCI layer
		source = compressed
	}
	reader := tar.NewReader(source)
	seen := map[string]bool{}
	resolvedBeforeLayer := make(map[string]bool, len(files))
	for name := range files {
		resolvedBeforeLayer[name] = true
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read OCI layer archive: %w", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name == "" {
			if header.Name == "./" && header.Typeflag == tar.TypeDir {
				continue
			}
			return fmt.Errorf("OCI layer has unsafe path %q", header.Name)
		}
		if strings.HasPrefix(name, "/") || pathpkg.Clean(name) != name {
			return fmt.Errorf("OCI layer has unsafe path %q", header.Name)
		}
		absoluteName := "/" + name
		if limit, ok := wanted[absoluteName]; ok {
			if seen[absoluteName] {
				return fmt.Errorf("OCI layer repeats %s", absoluteName)
			}
			seen[absoluteName] = true
			if _, resolved := files[absoluteName]; resolved {
				continue
			}
			if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > limit {
				return fmt.Errorf("project image rootfs has invalid %s", absoluteName)
			}
			body, err := io.ReadAll(io.LimitReader(reader, limit+1))
			if err != nil || int64(len(body)) != header.Size {
				return fmt.Errorf("project image rootfs truncated %s", absoluteName)
			}
			files[absoluteName] = body
			modes[absoluteName] = header.Mode
			continue
		}
		for target := range wanted {
			if resolvedBeforeLayer[target] {
				continue
			}
			if name == ".wh..wh..opq" {
				return fmt.Errorf("project image rootfs opaquely hides %s", target)
			}
			relativeTarget := strings.TrimPrefix(target, "/")
			parent := pathpkg.Dir(relativeTarget)
			whiteout := pathpkg.Join(parent, ".wh."+pathpkg.Base(relativeTarget))
			if name == whiteout {
				return fmt.Errorf("project image rootfs whiteouts %s", target)
			}
			for ancestor := parent; ancestor != "." && ancestor != "/"; ancestor = pathpkg.Dir(ancestor) {
				ancestorWhiteout := pathpkg.Join(
					pathpkg.Dir(ancestor), ".wh."+pathpkg.Base(ancestor))
				if name == ancestorWhiteout {
					return fmt.Errorf(
						"project image rootfs whiteouts ancestor of %s", target)
				}
				if name == pathpkg.Join(ancestor, ".wh..wh..opq") {
					return fmt.Errorf("project image rootfs opaquely hides %s", target)
				}
				if name == ancestor && header.Typeflag != tar.TypeDir {
					return fmt.Errorf("project image rootfs replaces ancestor of %s", target)
				}
			}
		}
	}
}

func (a appleBackend) ensureRegistryImage(ctx context.Context) error {
	digest, err := a.ImageDigest(ctx, registryImage)
	if err == nil && digest == registryImageDigest {
		return nil
	}
	output, pullErr := a.runner.Run(ctx, commandSpec{
		Path: a.containerPath, Args: []string{"image", "pull", "--scheme", "https", registryImage},
	})
	if pullErr != nil {
		return runError("pull pinned registry helper", output, pullErr)
	}
	digest, err = a.ImageDigest(ctx, registryImage)
	if err != nil {
		return err
	}
	if digest != registryImageDigest {
		return fmt.Errorf("registry helper digest %s does not match %s", digest, registryImageDigest)
	}
	return nil
}

func waitForRegistry(ctx context.Context, url string) error {
	for range 30 {
		if err := probeRegistry(ctx, url); err == nil {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("temporary registry did not become ready")
}

func probeRegistry(ctx context.Context, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // readiness status is primary
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("registry readiness probe returned %s", response.Status)
	}
	return nil
}

// deleteRegistryManifest removes a failed publication's manifest, and with
// it any tags, from a loopback registry. Deletion support is a creation-time
// registry property: a retained registry created before the builder enabled
// it answers 405, which is accepted retention rather than an error, so one
// legacy registry cannot poison every later failure path on its port (#352).
// 404 means the residue is already gone.
func deleteRegistryManifest(ctx context.Context, url string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // deletion status is primary
	switch response.StatusCode {
	case http.StatusAccepted, http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil
	}
	return fmt.Errorf("registry manifest deletion returned %s", response.Status)
}
