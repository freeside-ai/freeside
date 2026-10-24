package claudeinference

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// probeSiteID names the probe's reservation of the driver's one native call.
// It is no inference site: nothing resolves it, and promptFor refuses it.
const probeSiteID = "lineup_probe"

const (
	// ProbePrompt and ProbeInput are the probe's whole conversation: one
	// fixed system prompt and one fixed message, so the call costs the same
	// every time and carries nothing from a task.
	ProbePrompt = "You are checking that this model answers. Reply with the single word OK."
	ProbeInput  = "Reply with the single word OK."
	// probeTimeout bounds the call, and probeVersionTimeout the --version
	// run before it. A refusal comes back in about a second; the bound
	// leaves room for a slow first token at a high effort.
	probeTimeout        = 2 * time.Minute
	probeVersionTimeout = 30 * time.Second
	// probeMaxOutputTokens caps what the CLI may generate. It is the
	// CLAUDE_CODE_MAX_OUTPUT_TOKENS the call runs with, so a model that
	// thinks at its effort still has room to answer.
	probeMaxOutputTokens = 4096
	// probeOutputLimit bounds each captured stream. A refusal or a one-word
	// JSON envelope is a few kilobytes.
	probeOutputLimit = 64 << 10
)

// ProbeOutput is what one probe call printed, unjudged: the CLI's --version
// line, the call's stdout and stderr, and its exit status. The caller judges
// it, so the host binary and the image probes share one verdict.
type ProbeOutput struct {
	Version    []byte
	Stdout     []byte
	Stderr     []byte
	ExitStatus int
}

// Probe makes one judgment launch with a fixed prompt: the verified private
// copy of the pinned CLI, the argument list and environment Complete uses,
// and the model and effort a judgment line names. It answers whether that
// CLI serves that model at that effort for this credential, which Complete
// cannot say, since it collapses every failure into errCompletion.
//
// The error is for a probe that could not run: an invalid launch value, a
// changed binary, a busy driver, an overlong stream, or a timeout. A CLI that
// ran and refused returns its output and a nil error.
func (d *Driver) Probe(ctx context.Context, model, nativeEffort string, credential inference.Secret) (out ProbeOutput, err error) {
	if model == "" || !inference.ValidLaunchValue(model) || !inference.ValidLaunchValue(nativeEffort) ||
		credential.Reveal() == "" {
		return ProbeOutput{}, errors.New("claude probe needs a credential and valid launch values")
	}
	callCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	call, err := d.acquire(callCtx, probeSiteID, cancel)
	if err != nil {
		return ProbeOutput{}, errors.New("claude probe: driver busy")
	}
	defer d.release(call)
	root, err := os.MkdirTemp("", "freeside-lineup-probe-")
	if err != nil {
		return ProbeOutput{}, errors.New("claude probe: create scratch directory")
	}
	defer func() {
		if cleanupErr := os.RemoveAll(root); cleanupErr != nil {
			out, err = ProbeOutput{}, errors.New("claude probe: remove scratch directory")
		}
	}()
	binary := filepath.Join(root, "claude")
	if err := d.writePrivateBinary(binary); err != nil {
		return ProbeOutput{}, err
	}
	// The --version run gets no credential: it reads nothing from a
	// provider, and the call below is the only one the token is for.
	versionCtx, cancelVersion := context.WithTimeout(callCtx, probeVersionTimeout)
	version := &boundedOutput{limit: probeOutputLimit, cancel: cancelVersion}
	versionCmd := exec.CommandContext(versionCtx, binary, "--version") //nolint:gosec // G204: private content-pinned CLI copy, fixed argument.
	versionCmd.Dir = root
	versionCmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "CLAUDE_CONFIG_DIR=" + root, "DISABLE_AUTOUPDATER=1"}
	versionCmd.Stdout = version
	versionErr := procbound.Run(versionCmd, time.Second)
	cancelVersion()
	if versionErr != nil {
		return ProbeOutput{}, errors.New("claude probe: --version failed")
	}
	cmd := exec.CommandContext(callCtx, binary, commandArgs(model, nativeEffort, ProbePrompt)...) //nolint:gosec // G204: private content-pinned CLI copy; model and effort checked above; no shell.
	cmd.Dir = root
	cmd.Env = commandEnv(root, credential,
		inference.Request{MaxComputeUnits: probeMaxOutputTokens}, inference.Site{Timeout: probeTimeout})
	cmd.Stdin = bytes.NewReader([]byte(ProbeInput))
	stdout := &boundedOutput{limit: probeOutputLimit, cancel: cancel}
	stderr := &boundedOutput{limit: probeOutputLimit, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := procbound.Run(cmd, time.Second)
	if callCtx.Err() != nil {
		return ProbeOutput{}, errors.New("claude probe: call timed out or overflowed its output bound")
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return ProbeOutput{}, errors.New("claude probe: run CLI")
	}
	return ProbeOutput{
		Version: version.Bytes(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		ExitStatus: cmd.ProcessState.ExitCode(),
	}, nil
}
