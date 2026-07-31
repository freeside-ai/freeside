package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type verificationCommand func(
	context.Context, string, []string, int64,
) (verify.StepResult, error)

const verificationCleanupTimeout = 2 * time.Minute

// ProjectRecipePath is the fixed location where the project-image builder
// embeds the exact verification-recipe bytes approved during onboarding.
const ProjectRecipePath = "/usr/local/share/freeside/project-recipe.json"

// ProjectImageRoom runs trusted verification commands in the admitted
// project image with no network. Every one-shot container carries an
// unpredictable ownership label and is reaped through fresh ownership
// evidence after success, failure, or cancellation.
type ProjectImageRoom struct {
	containerPath string
	image         domain.ProjectImage
	runtime       Runtime
	runCommand    verificationCommand
	readCommand   verificationCommand
	maxOutput     int64
}

// NewProjectImageRoom constructs the production verification room.
func NewProjectImageRoom(containerPath string, image domain.ProjectImage) (*ProjectImageRoom, error) {
	if err := image.Validate(); err != nil {
		return nil, fmt.Errorf("project-image verification room: %w", err)
	}
	if containerPath == "" {
		containerPath = "container"
	}
	resolved, err := exec.LookPath(containerPath)
	if err != nil {
		return nil, fmt.Errorf("resolve container executable %q: %w", containerPath, err)
	}
	return newProjectImageRoom(
		resolved, image, NewCLIRuntime(resolved), runVerificationCommand, runRecipeReadCommand,
		verify.DefaultMaxRoomOutputBytes,
	), nil
}

func newProjectImageRoom(
	containerPath string,
	image domain.ProjectImage,
	runtime Runtime,
	runCommand verificationCommand,
	readCommand verificationCommand,
	maxOutput int64,
) *ProjectImageRoom {
	return &ProjectImageRoom{
		containerPath: containerPath, image: image, runtime: runtime,
		runCommand: runCommand, readCommand: readCommand, maxOutput: maxOutput,
	}
}

var _ verify.Room = (*ProjectImageRoom)(nil)

// ReadRecipe extracts the onboarding-bound recipe from the admitted image.
// The extraction gets no workspace mount, credentials, or network, and the
// bytes must match the recipe digest carried by the durable ProjectImage
// before the caller may execute them.
func (r *ProjectImageRoom) ReadRecipe(ctx context.Context) ([]byte, error) {
	if r == nil || r.runtime == nil || r.readCommand == nil || r.containerPath == "" {
		return nil, errors.New("project-image verification room is not initialized")
	}
	result, err := r.runImageCommand(
		ctx, "", []string{"/bin/cat", ProjectRecipePath}, verify.DefaultMaxRecipeBytes,
		r.readCommand,
	)
	if err != nil {
		return nil, fmt.Errorf("extract project-image verification recipe: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("extract project-image verification recipe: container exited %d", result.ExitCode)
	}
	if result.Truncated {
		return nil, fmt.Errorf("project-image verification recipe exceeds the %d-byte cap",
			verify.DefaultMaxRecipeBytes)
	}
	if got := verify.RecipeDigest(result.Output); got != r.image.RecipeDigest {
		return nil, fmt.Errorf("project-image verification recipe digest %s, want %s: %w",
			got, r.image.RecipeDigest, domain.ErrParentKeyMismatch)
	}
	return bytes.Clone(result.Output), nil
}

// Run executes image-owned preparation and then the recipe argv in separate
// networkless containers over the same fresh verifier workspace.
func (r *ProjectImageRoom) Run(
	ctx context.Context,
	workdir string,
	argv []string,
) (verify.StepResult, error) {
	if r == nil || r.runtime == nil || r.runCommand == nil || r.containerPath == "" {
		return verify.StepResult{}, errors.New("project-image verification room is not initialized")
	}
	workspace, err := authenticateVerificationWorkspace(workdir)
	if err != nil {
		return verify.StepResult{}, err
	}
	if err := validateVerificationArgv(argv); err != nil {
		return verify.StepResult{}, err
	}
	prepared, err := r.runContainer(ctx, workspace, r.image.PreparationCommand)
	if err != nil || prepared.ExitCode != 0 {
		return prepared, err
	}
	return r.runContainer(ctx, workspace, argv)
}

func (r *ProjectImageRoom) runContainer(
	ctx context.Context,
	workspace string,
	argv []string,
) (result verify.StepResult, err error) {
	return r.runImageCommand(ctx, workspace, argv, r.maxOutput, r.runCommand)
}

func (r *ProjectImageRoom) runImageCommand(
	ctx context.Context,
	workspace string,
	argv []string,
	maxOutput int64,
	command verificationCommand,
) (result verify.StepResult, err error) {
	owner, err := newOwnershipLabel()
	if err != nil {
		return verify.StepResult{}, err
	}
	cidPath, err := newVerificationCIDPath()
	if err != nil {
		return verify.StepResult{}, err
	}
	defer os.Remove(cidPath) //nolint:errcheck // private one-shot runtime identity
	args := []string{
		"run", "--cidfile", cidPath,
		"--label", owner.Key + "=" + owner.Value,
		"--network", "none",
	}
	if workspace != "" {
		args = append(args,
			"--volume", workspace+":/workspace",
			"--workdir", "/workspace",
			"--env", "HOME=/tmp/freeside-home",
			"--env", "LC_ALL=C",
		)
	}
	args = append(args, "--", string(r.image.ImageRef))
	args = append(args, argv...)
	result, runErr := command(ctx, r.containerPath, args, maxOutput)
	id, identityErr := readVerificationContainerID(cidPath)
	cleanupCtx, cancelCleanup := context.WithTimeout(
		context.WithoutCancel(ctx), verificationCleanupTimeout,
	)
	cleanupErr := r.cleanupOwnedContainers(cleanupCtx, id, owner)
	cancelCleanup()
	if cleanupErr != nil {
		result = verify.StepResult{}
	}
	if identityErr != nil && runErr == nil {
		return verify.StepResult{}, errors.Join(identityErr, cleanupErr)
	}
	return result, errors.Join(runErr, cleanupErr)
}

func (r *ProjectImageRoom) cleanupOwnedContainers(
	ctx context.Context,
	id string,
	owner Label,
) error {
	containers, err := r.runtime.ListContainers(ctx)
	if err != nil {
		if id == "" {
			return fmt.Errorf("list verification containers for ownership recovery: %w", err)
		}
		backend := &Backend{rt: r.runtime}
		return backend.reapUnlistedContainer(ctx, id, objectClaim{attempted: true}, owner)
	}
	backend := &Backend{rt: r.runtime}
	var cleanupErr error
	for _, candidate := range containers {
		if !validVerificationContainerID(candidate.ID) {
			cleanupErr = errors.Join(cleanupErr,
				errors.New("verification ownership recovery observed an invalid container identity"))
			continue
		}
		if candidate.LabelsObserved && !slices.Contains(candidate.Labels, owner) && candidate.ID != id {
			continue
		}
		evidence, evidenceErr := backend.containerEvidence(
			ctx, candidate, objectClaim{attempted: true}, owner,
		)
		if evidenceErr != nil {
			cleanupErr = errors.Join(cleanupErr, evidenceErr)
			continue
		}
		switch evidence {
		case evidenceOurs:
			cleanupErr = errors.Join(cleanupErr, backend.reapContainer(ctx, candidate))
		case evidenceForeign:
			if candidate.ID == id {
				cleanupErr = errors.Join(cleanupErr,
					errors.New("runtime identity file names a foreign verification container"))
			}
		case evidenceUnprovable:
			cleanupErr = errors.Join(cleanupErr,
				errors.New("verification container ownership is unprovable; not deleting"))
		}
	}
	return cleanupErr
}

func runVerificationCommand(
	ctx context.Context,
	path string,
	args []string,
	maxOutput int64,
) (verify.StepResult, error) {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // executable resolved at construction; argv is trusted recipe/project-image data
	output := &verificationOutput{max: maxOutput}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	result := verify.StepResult{Output: output.buf.Bytes(), Truncated: output.truncated}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		if ctx.Err() != nil || result.ExitCode < 0 {
			result.ExitCode = -1
		}
		return result, nil
	}
	if ctx.Err() != nil {
		result.ExitCode = -1
		return result, nil
	}
	return verify.StepResult{}, err
}

// runRecipeReadCommand keeps Apple container's progress diagnostics on stderr
// out of the recipe byte stream. Verification commands intentionally combine
// stdout and stderr into their transcript; recipe authentication requires the
// exact file bytes from stdout alone.
func runRecipeReadCommand(
	ctx context.Context,
	path string,
	args []string,
	maxOutput int64,
) (verify.StepResult, error) {
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // executable and fixed extraction argv are resolved by the room
	stdout := &verificationOutput{max: maxOutput}
	stderr := &verificationOutput{max: maxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if stderr.truncated {
		return verify.StepResult{}, fmt.Errorf("recipe extraction diagnostics exceeded the %d-byte cap", maxOutput)
	}
	result := verify.StepResult{Output: stdout.buf.Bytes(), Truncated: stdout.truncated}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		if ctx.Err() != nil || result.ExitCode < 0 {
			result.ExitCode = -1
		}
		return result, nil
	}
	if ctx.Err() != nil {
		result.ExitCode = -1
		return result, nil
	}
	return verify.StepResult{}, err
}

func authenticateVerificationWorkspace(workdir string) (string, error) {
	if workdir == "" || !filepath.IsAbs(workdir) || filepath.Clean(workdir) != workdir ||
		strings.ContainsAny(workdir, ":\x00\r\n") {
		return "", errors.New("verification workspace is not a clean absolute container-volume source")
	}
	info, err := os.Lstat(workdir)
	if err != nil {
		return "", fmt.Errorf("inspect verification workspace: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("verification workspace is not a real directory")
	}
	resolved, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return "", fmt.Errorf("resolve verification workspace: %w", err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(info, resolvedInfo) {
		return "", fmt.Errorf("verification workspace identity changed: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	return resolved, nil
}

func validateVerificationArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("project-image verification command is empty")
	}
	for index, token := range argv {
		if strings.ContainsRune(token, 0) {
			return fmt.Errorf("verification argv[%d] contains NUL", index)
		}
	}
	return nil
}

func newVerificationCIDPath() (string, error) {
	file, err := os.CreateTemp("", "freeside-verification-container-id-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func readVerificationContainerID(path string) (string, error) {
	body, err := os.ReadFile(path) //nolint:gosec // private path from newVerificationCIDPath
	if err != nil {
		return "", fmt.Errorf("read verification container identity: %w", err)
	}
	id := strings.TrimSpace(string(body))
	if !validVerificationContainerID(id) {
		return "", errors.New("runtime returned an invalid verification container identity")
	}
	return id, nil
}

func validVerificationContainerID(id string) bool {
	return len(id) > 0 && len(id) <= 128 &&
		((id[0] >= 'a' && id[0] <= 'z') ||
			(id[0] >= 'A' && id[0] <= 'Z') ||
			(id[0] >= '0' && id[0] <= '9')) &&
		strings.IndexFunc(id, func(r rune) bool {
			return (r < 'a' || r > 'z') &&
				(r < 'A' || r > 'Z') &&
				(r < '0' || r > '9') && r != '-' && r != '_' && r != '.'
		}) == -1
}

type verificationOutput struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func (o *verificationOutput) Write(body []byte) (int, error) {
	if remaining := o.max - int64(o.buf.Len()); remaining > 0 {
		keep := body
		if int64(len(keep)) > remaining {
			keep = keep[:remaining]
			o.truncated = true
		}
		_, _ = o.buf.Write(keep)
	} else if len(body) > 0 {
		o.truncated = true
	}
	return len(body), nil
}
