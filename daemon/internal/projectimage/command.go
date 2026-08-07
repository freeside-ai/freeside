package projectimage

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

const maxCommandOutputBytes = 1 << 20

type commandSpec struct {
	Path string
	Args []string
	Dir  string
	Env  []string
}

type commandRunner interface {
	Run(context.Context, commandSpec) (commandOutput, error)
}

type execRunner struct{}

type commandOutput struct {
	bytes     []byte
	truncated bool
	exited    bool
	exitCode  int
}

func (execRunner) Run(ctx context.Context, spec commandSpec) (commandOutput, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...) //nolint:gosec // paths are operator-resolved executables; arguments are fixed or validated opaque argv
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = append([]string{}, spec.Env...)
	}
	output := tailBuffer{max: maxCommandOutputBytes}
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := procbound.Run(cmd, procbound.DefaultWaitDelay)
	result := commandOutput{bytes: output.bytes(), truncated: output.truncated}
	if cmd.ProcessState != nil {
		result.exited = true
		result.exitCode = cmd.ProcessState.ExitCode()
	}
	return result, err
}

func resolveExecutable(configured, fallback string) (string, error) {
	name := configured
	if name == "" {
		name = fallback
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("resolve %s executable %q: %w", fallback, name, err)
	}
	return path, nil
}

// tailBuffer bounds external tool output while retaining the failure detail
// commands conventionally print at the end. Write always reports full
// consumption so a noisy child cannot block on its own diagnostics.
type tailBuffer struct {
	buf       []byte
	max       int
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.max {
		b.truncated = b.truncated || len(p) > b.max || len(b.buf) > 0
		b.buf = append(b.buf[:0], p[len(p)-b.max:]...)
		return n, nil
	}
	overflow := len(b.buf) + len(p) - b.max
	if overflow > 0 {
		b.truncated = true
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *tailBuffer) bytes() []byte {
	return append([]byte{}, b.buf...)
}

func runError(action string, output commandOutput, err error) error {
	if err == nil {
		return nil
	}
	if tail := boundedOutput(output.bytes); tail != "" {
		return fmt.Errorf("%s: %w\n%s", action, err, tail)
	}
	return fmt.Errorf("%s: %w", action, err)
}
