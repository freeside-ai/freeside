package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// DefaultMaxRoomOutputBytes bounds the combined output one command may
// retain when the room's cap is unset. Zero selects the default and a
// negative cap is a caller bug; an accidentally unbounded room must not
// buffer a hostile test suite's output without limit.
const DefaultMaxRoomOutputBytes = 1 << 20

// DefaultRoomWaitDelay bounds how long Run waits for the command's I/O
// pipes after the process exits or the context is done. Without it a
// grandchild holding the output pipe (a background process the
// candidate's tests left behind) keeps Run blocked arbitrarily long and
// defeats the per-command timeout (refute-pass finding, confirmed by
// probe).
const DefaultRoomWaitDelay = 10 * time.Second

// ProcRoom is the process-level room this unit ships instead of real
// container execution (an explicit non-goal; ward provides real rooms).
// It executes argv directly, never through a shell, with a scrubbed
// allowlist environment: PATH to find toolchains, a scratch HOME so the
// daemon user's real home and its credential stores are invisible, and
// LC_ALL=C. It is honest about its isolation class: a process-level
// room cannot deny network or filesystem access, and its process-group
// reap (below) misses a descendant that deliberately escapes the group
// with setsid or its own new session, which can then outlive the step
// on the host. Containing an escaping descendant needs a real
// containment primitive (a PID namespace or cgroup reaper), which is
// the ward room's job, not a process-level fake's: this is a weaker
// class than the ward's room (§5.7's no-silent-downgrade discipline),
// suitable for tests and bring-up, never a substitute where ward
// isolation is required. The Room interface exists so the ward room
// replaces this backend without moving any trust logic.
type ProcRoom struct {
	// Home is the scratch directory the child sees as HOME. Required:
	// an empty Home fails loud rather than inheriting the real one.
	Home string
	// MaxOutputBytes bounds the combined output retained per command;
	// zero selects DefaultMaxRoomOutputBytes.
	MaxOutputBytes int64
	// WaitDelay bounds the wait for I/O pipes after process exit or
	// context cancellation; zero selects DefaultRoomWaitDelay.
	WaitDelay time.Duration
	// Invocations records every executed command, in order, for tests
	// and the verification account. ProcRoom is not safe for concurrent
	// Run calls; recipe commands run sequentially.
	Invocations []Invocation
}

// Invocation is one recorded command execution.
type Invocation struct {
	Argv []string
	Dir  string
	Env  []string
}

// Run executes one recipe command in workdir. A non-zero exit or a
// signal death (including a timeout kill) is a StepResult, not an
// error: a failing candidate is a verification outcome. An error means
// the room itself could not execute the command.
func (r *ProcRoom) Run(ctx context.Context, workdir string, argv []string) (StepResult, error) {
	if r.Home == "" {
		return StepResult{}, fmt.Errorf("proc room home is unset: %w", ErrInvalidOptions)
	}
	if len(argv) == 0 {
		return StepResult{}, fmt.Errorf("empty command: %w", ErrInvalidOptions)
	}
	maxBytes := r.MaxOutputBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxRoomOutputBytes
	}
	if maxBytes < 0 {
		return StepResult{}, fmt.Errorf("negative room output cap: %w", ErrInvalidOptions)
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.Home,
		"LC_ALL=C",
	}
	waitDelay := r.WaitDelay
	if waitDelay == 0 {
		waitDelay = DefaultRoomWaitDelay
	}
	if waitDelay < 0 {
		return StepResult{}, fmt.Errorf("negative room wait delay: %w", ErrInvalidOptions)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv comes from the parsed trusted recipe, never candidate bytes
	cmd.Dir = workdir
	cmd.Env = env
	cmd.WaitDelay = waitDelay
	// Run the command as its own process group and kill the whole group,
	// on cancellation and unconditionally once the step is over:
	// candidate test code runs here, and a background descendant it
	// leaves behind must not outlive the step on the host (WaitDelay
	// unblocks Run but reaps nothing by itself).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	out := &boundedBuffer{max: maxBytes}
	cmd.Stdout, cmd.Stderr = out, out
	r.Invocations = append(r.Invocations, Invocation{Argv: argv, Dir: workdir, Env: env})
	err := cmd.Run()
	if cmd.Process != nil {
		// Best-effort group reap; ESRCH just means nothing lingered.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	res := StepResult{Output: out.buf.Bytes(), Truncated: out.truncated}
	if err == nil {
		return res, nil
	}
	// A process that exited cleanly while something it spawned still
	// holds the output pipe is not a clean verification: the step fails
	// rather than letting a lingering background process read as passed.
	if errors.Is(err, exec.ErrWaitDelay) {
		res.ExitCode = -1
		return res, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	return StepResult{}, fmt.Errorf("room could not execute %q: %w", argv[0], err)
}

// boundedBuffer keeps the first max bytes and drops the rest, reporting
// the writes as fully consumed so the child never blocks on a full pipe.
type boundedBuffer struct {
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.max - int64(b.buf.Len()); remaining > 0 {
		keep := p
		if int64(len(keep)) > remaining {
			keep = keep[:remaining]
			b.truncated = true
		}
		b.buf.Write(keep)
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}
