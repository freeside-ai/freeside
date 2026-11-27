package projectimage

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// Agent-base upkeep (plan §5.7) builds the Claude agent base from
// domain.AgentBaseSourceDir at the agent_base source repository's merged
// tip, probes it, and publishes it beside the project image it will serve.
// Review at merge is the gate on what the directory's Containerfile runs;
// the build only refuses a Containerfile whose base images are not digest
// pinned, and the probes refuse an image that does not carry the CLI the
// daemon pins or fails the ward's allowlist.

// AgentBaseImageName is the image name the daemon publishes an agent base
// under and tags it with locally.
const AgentBaseImageName = "freeside-agent-claude"

// claudeVersionArg is the Containerfile argument that pins the Claude CLI.
const claudeVersionArg = "CLAUDE_CODE_VERSION"

// maxContainerfileBytes bounds the Containerfile read from a commit.
const maxContainerfileBytes = 64 << 10

// ErrAgentBaseSource marks a source tree the daemon will not build a base
// from: no source directory, or a Containerfile without digest-pinned base
// images or a readable CLI pin.
var ErrAgentBaseSource = errors.New("agent base source is not buildable")

// claudeVersionPattern bounds what a pin may be before it reaches a build
// argument or a comparison.
var claudeVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// AgentBaseLocalTag is the local tag a project-image build resolves an
// admitted base through (Request.BaseBuildRef). It is derived from the
// base's digest, so it names exactly one base and a project rebuild can
// find the tag of any base the daemon admitted.
func AgentBaseLocalTag(ref domain.ImageRef) string {
	hex := contentaddr.Hex(imageDigest(ref))
	if hex == "" {
		// Not a digest-pinned reference: no tag, which every build refuses.
		return ""
	}
	return AgentBaseImageName + ":admitted-" + hex[:12]
}

// AgentBaseSource is what a commit's source directory pins.
type AgentBaseSource struct {
	// TreeSHA is the git tree of domain.AgentBaseSourceDir.
	TreeSHA string
	// ClaudeVersion is the Containerfile's CLAUDE_CODE_VERSION default.
	ClaudeVersion string
}

// ReadAgentBaseSource reads domain.AgentBaseSourceDir at commitSHA in the
// checkout: its tree, and the CLI pin of a Containerfile whose base images
// are all digest pinned. A commit without the directory, or with a
// Containerfile that fails either rule, is ErrAgentBaseSource.
func ReadAgentBaseSource(ctx context.Context, gitPath, checkoutDir, commitSHA string) (AgentBaseSource, error) {
	reader, err := verify.OpenCommitReader(ctx, gitPath, checkoutDir)
	if err != nil {
		return AgentBaseSource{}, fmt.Errorf("read agent base source at %s: %w", commitSHA, err)
	}
	defer reader.Close()
	tree, present, err := reader.TreeAt(ctx, commitSHA, domain.AgentBaseSourceDir)
	if err != nil {
		return AgentBaseSource{}, fmt.Errorf("read agent base source at %s: %w", commitSHA, err)
	}
	if !present {
		return AgentBaseSource{}, fmt.Errorf("%s holds no %s: %w", commitSHA, domain.AgentBaseSourceDir, ErrAgentBaseSource)
	}
	containerfile, present, err := reader.ReadFile(
		ctx, commitSHA, domain.AgentBaseSourceDir+"/Containerfile", maxContainerfileBytes)
	if err != nil {
		return AgentBaseSource{}, fmt.Errorf("read agent base Containerfile at %s: %w", commitSHA, err)
	}
	if !present {
		return AgentBaseSource{}, fmt.Errorf("%s holds no %s/Containerfile: %w",
			commitSHA, domain.AgentBaseSourceDir, ErrAgentBaseSource)
	}
	version, err := checkAgentBaseContainerfile(containerfile)
	if err != nil {
		return AgentBaseSource{}, fmt.Errorf("agent base Containerfile at %s: %w", commitSHA, err)
	}
	return AgentBaseSource{TreeSHA: tree, ClaudeVersion: version}, nil
}

// parserDirective matches BuildKit's syntax directive, which names a
// frontend image the build pulls and runs, and its escape directive, which
// changes the continuation character logicalLines joins on.
var parserDirective = regexp.MustCompile(`(?i)^#\s*(syntax|escape)\s*=`)

// checkAgentBaseContainerfile returns the CLI pin of a Containerfile whose
// every image reference (a FROM, a COPY or ADD --from, or a RUN --mount
// from) names an earlier stage or a digest-pinned image, directly or
// through an ARG default declared before the first FROM, and that names no
// syntax frontend or escape character. A build argument could still
// override such a default; the daemon's base build passes none but the
// proxy's.
func checkAgentBaseContainerfile(content []byte) (string, error) {
	globals := map[string]string{}
	stages := map[string]bool{}
	pinnedImage := func(image string) error {
		if stages[strings.ToLower(image)] {
			return nil
		}
		resolved, err := resolveFromImage(image, globals)
		if err != nil {
			return err
		}
		if err := domain.ImageRef(resolved).Validate(); err != nil {
			return fmt.Errorf("image %s is not digest pinned: %w", image, ErrAgentBaseSource)
		}
		return nil
	}
	version := ""
	seenFrom := false
	lines, err := logicalLines(content)
	if err != nil {
		return "", err
	}
	for _, line := range lines {
		if parserDirective.MatchString(line) {
			return "", fmt.Errorf("the Containerfile names a parser directive: %w", ErrAgentBaseSource)
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "COPY", "ADD", "RUN":
			for _, flag := range fields[1:] {
				if !strings.HasPrefix(flag, "--") {
					break
				}
				for _, image := range flagImages(flag) {
					if err := pinnedImage(image); err != nil {
						return "", err
					}
				}
			}
		case "ARG":
			for _, declaration := range fields[1:] {
				name, value, hasDefault := strings.Cut(declaration, "=")
				if !hasDefault {
					continue
				}
				if !seenFrom {
					globals[name] = value
				}
				if name == claudeVersionArg {
					if version != "" && version != value {
						return "", fmt.Errorf("%s has two defaults, %q and %q: %w",
							claudeVersionArg, version, value, ErrAgentBaseSource)
					}
					version = value
				}
			}
		case "FROM":
			seenFrom = true
			args := fields[1:]
			for len(args) > 0 && strings.HasPrefix(args[0], "--") {
				args = args[1:]
			}
			if len(args) == 0 {
				return "", fmt.Errorf("FROM names no image: %w", ErrAgentBaseSource)
			}
			if err := pinnedImage(args[0]); err != nil {
				return "", err
			}
			if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
				stages[strings.ToLower(args[2])] = true
			}
		}
	}
	if !claudeVersionPattern.MatchString(version) {
		return "", fmt.Errorf("%s default %q is not a release version: %w", claudeVersionArg, version, ErrAgentBaseSource)
	}
	return version, nil
}

// logicalLines returns the Containerfile's instructions with each one's
// backslash-continued lines joined, as the builder joins them before it
// reads the instruction, so a flag on a continued line is checked with the
// instruction it belongs to. A comment or blank line inside a continuation
// is dropped, as the builder drops it; a comment outside one is its own
// line, so a directive is still seen.
func logicalLines(content []byte) ([]string, error) {
	var (
		lines   []string
		pending []string
	)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			if len(pending) == 0 {
				lines = append(lines, line)
			}
			continue
		}
		part, continued := strings.CutSuffix(line, `\`)
		pending = append(pending, part)
		if !continued {
			lines = append(lines, strings.Join(pending, " "))
			pending = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan Containerfile: %w", err)
	}
	if len(pending) > 0 {
		lines = append(lines, strings.Join(pending, " "))
	}
	return lines, nil
}

// flagImages returns the images an instruction flag reads from: the value
// of --from, or the from option of a --mount.
func flagImages(flag string) []string {
	if image, ok := strings.CutPrefix(flag, "--from="); ok {
		return []string{image}
	}
	spec, ok := strings.CutPrefix(flag, "--mount=")
	if !ok {
		return nil
	}
	var images []string
	for _, option := range strings.Split(spec, ",") {
		if image, ok := strings.CutPrefix(option, "from="); ok {
			images = append(images, image)
		}
	}
	return images
}

// resolveFromImage expands a FROM image that is wholly one ARG reference.
func resolveFromImage(image string, globals map[string]string) (string, error) {
	name := ""
	switch {
	case strings.HasPrefix(image, "${") && strings.HasSuffix(image, "}"):
		name = image[2 : len(image)-1]
	case strings.HasPrefix(image, "$"):
		name = image[1:]
	default:
		if strings.Contains(image, "$") {
			return "", fmt.Errorf("FROM %s mixes text and arguments: %w", image, ErrAgentBaseSource)
		}
		return image, nil
	}
	value, ok := globals[name]
	if !ok || strings.Contains(value, "$") {
		return "", fmt.Errorf("FROM %s has no literal default before the first FROM: %w", image, ErrAgentBaseSource)
	}
	return value, nil
}

// AgentBaseRequest is one agent-base build.
type AgentBaseRequest struct {
	// SourceDir is a git checkout holding CommitSHA.
	SourceDir string
	CommitSHA string
	// ClaudeVersion is the CLI version the daemon pins. The Containerfile
	// must pin it and the built image must report it.
	ClaudeVersion string
	// Destination is an image in the registry the base publishes to: the
	// project image the base will serve.
	Destination domain.ImageRef
	DNS         []string
	BuildProxy  string
}

// AgentBase is a built, probed, and published base.
type AgentBase struct {
	ImageRef domain.ImageRef
	// ClaudeVersion is the version `claude --version` reported in the image.
	ClaudeVersion string
	// LocalTag is AgentBaseLocalTag(ImageRef), left tagged on the host.
	LocalTag string
}

// BuildAgentBase builds the base from domain.AgentBaseSourceDir at the
// request's commit behind the build proxy, proves it, publishes it, and
// leaves AgentBaseLocalTag on the host for project rebuilds. It admits
// nothing: the caller records the base. The proofs, each refusing the base
// with ErrProofFailed:
//
//   - The ward's post-create allowlist, on the local and the published
//     image, as scripts/check-agent-image.sh checks an operator's base.
//   - `claude --version` with no network reports exactly the pinned
//     version, as scripts/build-agent-claude-image.sh asserts.
//   - The managed settings the image carries disable every built-in plugin
//     and are readable by the uid a launch drops to, as that script also
//     asserts.
func (b *Builder) BuildAgentBase(ctx context.Context, request AgentBaseRequest) (result AgentBase, err error) {
	if !commitPattern.MatchString(request.CommitSHA) || request.SourceDir == "" ||
		!claudeVersionPattern.MatchString(request.ClaudeVersion) {
		return AgentBase{}, fmt.Errorf("agent base request: %w", ErrInvalidRequest)
	}
	registry, localPort, err := publishedRegistry(request.Destination)
	if err != nil {
		return AgentBase{}, err
	}
	progress, err := startProgress(b.log, b.logDir, b.heartbeat, AgentBaseImageName)
	if err != nil {
		return AgentBase{}, err
	}
	defer progress.finish()
	scratch, err := os.MkdirTemp(b.tempDir, "freeside-agent-base-*")
	if err != nil {
		return AgentBase{}, fmt.Errorf("create agent-base scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // private scratch, best-effort after all handles close

	sourceStep := progress.begin("source materialization", request.CommitSHA)
	sourceDir := filepath.Join(scratch, "source")
	err = b.source.Copy(ctx, request.SourceDir, request.CommitSHA, sourceDir)
	sourceStep.end(err)
	if err != nil {
		return AgentBase{}, fmt.Errorf("materialize agent base source: %w", err)
	}
	contextDir := filepath.Join(sourceDir, filepath.FromSlash(domain.AgentBaseSourceDir))
	containerfile, err := os.ReadFile(filepath.Join(contextDir, "Containerfile")) //nolint:gosec // G304: the build's own scratch checkout
	if err != nil {
		return AgentBase{}, fmt.Errorf("read agent base Containerfile: %w: %w", err, ErrAgentBaseSource)
	}
	pinned, err := checkAgentBaseContainerfile(containerfile)
	if err != nil {
		return AgentBase{}, err
	}
	if pinned != request.ClaudeVersion {
		return AgentBase{}, fmt.Errorf("agent base Containerfile pins Claude Code %s, not the daemon's %s: %w",
			pinned, request.ClaudeVersion, ErrAgentBaseSource)
	}

	token, err := randomToken()
	if err != nil {
		return AgentBase{}, fmt.Errorf("generate agent base build reference: %w", err)
	}
	localRef := AgentBaseImageName + ":build-" + token
	buildStep := progress.beginCommand("container build", localRef+" at "+request.CommitSHA)
	err = b.backend.Build(ctx, buildSpec{
		AgentBase: true, ContextDir: contextDir, LocalRef: localRef,
		DNS: append([]string{}, request.DNS...), BuildProxy: request.BuildProxy,
		Output: buildStep.writer(),
	})
	buildStep.end(err)
	if err != nil {
		return AgentBase{}, fmt.Errorf("build agent base%s: %w", buildStep.noteSuffix(), err)
	}
	defer func() {
		if removeErr := b.backend.RemoveImage(context.WithoutCancel(ctx), localRef); removeErr != nil {
			result = AgentBase{}
			err = errors.Join(err, fmt.Errorf("remove local agent base build: %w", removeErr))
		}
	}()
	localDigest, err := b.backend.ImageDigest(ctx, localRef)
	if err != nil {
		return AgentBase{}, fmt.Errorf("inspect built agent base: %w", err)
	}
	if !validDigest(localDigest) {
		return AgentBase{}, fmt.Errorf("built agent base reported digest %q: %w", localDigest, ErrProofFailed)
	}
	allowlistStep := progress.begin("allowlist", "")
	err = b.backend.CheckAllowlist(ctx, localRef)
	allowlistStep.end(err)
	if err != nil {
		return AgentBase{}, fmt.Errorf("agent base allowlist: %w", proofFailure(err))
	}
	probeStep := progress.begin("probe", "")
	reported, err := b.probeAgentBase(ctx, localRef, request.ClaudeVersion)
	probeStep.end(err)
	if err != nil {
		return AgentBase{}, err
	}

	publishStep := progress.begin("publish", "")
	published, err := b.backend.Publish(ctx, publishSpec{
		LocalRef: localRef, Digest: localDigest, ImageName: AgentBaseImageName,
		RefTag: "base-" + request.CommitSHA[:12], Registry: registry, LocalRegistryPort: localPort,
		RefRecorded: b.lookupRecordedBase,
	})
	publishStep.end(err)
	if err != nil {
		return AgentBase{}, fmt.Errorf("publish agent base: %w", err)
	}
	// The same deferred order as Build: release the lease last, after any
	// owned registry is cleaned and residue discarded.
	defer func() {
		if published.release == nil {
			return
		}
		if releaseErr := published.release(); releaseErr != nil {
			result = AgentBase{}
			err = errors.Join(err, fmt.Errorf("release agent base publication: %w", releaseErr))
		}
	}()
	defer func() {
		if published.cleanup == nil {
			return
		}
		if cleanupErr := published.cleanup(context.WithoutCancel(ctx)); cleanupErr != nil {
			result = AgentBase{}
			err = errors.Join(err, fmt.Errorf("clean unpublished agent base: %w", cleanupErr))
		}
	}()
	defer func() {
		if published.discard == nil {
			return
		}
		if discardErr := published.discard(context.WithoutCancel(ctx)); discardErr != nil {
			result = AgentBase{}
			err = errors.Join(err, fmt.Errorf("discard unpublished agent base: %w", discardErr))
		}
	}()
	ref := published.Ref
	repository, _, _ := strings.Cut(string(ref), "@")
	if ref.Validate() != nil || repository != publishedRepository(registry, localPort, AgentBaseImageName) ||
		imageDigest(ref) != localDigest {
		return AgentBase{}, fmt.Errorf(
			"published agent base %q is not %s at the built digest %s: %w",
			ref, AgentBaseImageName, localDigest, ErrProofFailed)
	}
	publishedAllowlistStep := progress.begin("published allowlist", "")
	err = b.backend.CheckAllowlist(ctx, string(ref))
	publishedAllowlistStep.end(err)
	if err != nil {
		return AgentBase{}, fmt.Errorf("published agent base allowlist: %w", proofFailure(err))
	}
	tag := AgentBaseLocalTag(ref)
	if err := b.backend.PinBase(ctx, localRef, tag, localDigest); err != nil {
		return AgentBase{}, fmt.Errorf("tag admitted agent base: %w", err)
	}
	published.cleanup = nil
	published.discard = nil
	return AgentBase{ImageRef: ref, ClaudeVersion: reported, LocalTag: tag}, nil
}

// probeAgentBase runs the two offline probes and returns the reported CLI
// version.
func (b *Builder) probeAgentBase(ctx context.Context, ref, want string) (string, error) {
	run, err := b.backend.Run(ctx, runSpec{ImageRef: ref, Argv: []string{"claude", "--version"}})
	if err != nil {
		return "", fmt.Errorf("probe agent base CLI version: %w", err)
	}
	// `claude --version` prints "<version> (Claude Code)". The version is
	// compared whole, never as a substring, which would take 2.1.220 for
	// a pin of 2.1.2.
	fields := strings.Fields(string(run.Output))
	if run.ExitCode != 0 || len(fields) == 0 || fields[0] != want {
		return "", fmt.Errorf("agent base reports Claude Code %q (exit %d), not the pinned %s: %w",
			boundedProbeText(run.Output), run.ExitCode, want, ErrProofFailed)
	}
	settings, err := b.backend.Run(ctx, runSpec{ImageRef: ref, Argv: []string{
		"setpriv", "--reuid=1001", "--regid=1001", "--clear-groups",
		"jq", "-e", "(.enabledPlugins | length > 0) and all(.enabledPlugins[]; . == false)",
		"/etc/claude-code/managed-settings.json",
	}})
	if err != nil {
		return "", fmt.Errorf("probe agent base managed settings: %w", err)
	}
	if settings.ExitCode != 0 {
		return "", fmt.Errorf(
			"agent base managed settings are unreadable by the agent user or leave a built-in plugin enabled (exit %d): %w",
			settings.ExitCode, ErrProofFailed)
	}
	return fields[0], nil
}

// boundedProbeText keeps a probe's output short enough for an error.
func boundedProbeText(output []byte) string {
	text := strings.TrimSpace(string(output))
	if len(text) > 200 {
		return text[:200] + "..."
	}
	return text
}

// publishedRegistry parses the registry an image was published to: a
// loopback registry's port, or a registry host and path.
func publishedRegistry(ref domain.ImageRef) (registry string, localPort int, err error) {
	if err := ref.Validate(); err != nil {
		return "", 0, fmt.Errorf("publication destination: %w: %w", err, ErrInvalidRequest)
	}
	destination, _, _ := strings.Cut(string(ref), "@")
	separator := strings.LastIndex(destination, "/")
	if separator <= 0 {
		return "", 0, fmt.Errorf("image reference %q names no registry: %w", ref, ErrInvalidRequest)
	}
	registry = destination[:separator]
	if port, local := strings.CutPrefix(registry, "127.0.0.1:"); local {
		localPort, err = strconv.Atoi(port)
		if err != nil || localPort <= 0 {
			return "", 0, fmt.Errorf("image reference %q names a bad loopback port: %w", ref, ErrInvalidRequest)
		}
		return "", localPort, nil
	}
	return registry, 0, nil
}

// publishedRepository is the repository part a published reference to
// name in registry must carry.
func publishedRepository(registry string, localPort int, name string) string {
	if localPort != 0 {
		registry = "127.0.0.1:" + strconv.Itoa(localPort)
	}
	return strings.TrimSuffix(registry, "/") + "/" + name
}
