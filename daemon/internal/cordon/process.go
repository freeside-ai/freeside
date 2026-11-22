package cordon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// processSpec is one process a launch starts.
type processSpec struct {
	argv []string
	dir  string
	env  []string
	// stdin is copied to the process until it ends or the process stops
	// reading. Nil gives it /dev/null.
	stdin io.Reader
	// output receives stdout and stderr together. Nil sends them to
	// /dev/null.
	output io.Writer
}

// process is a started process. Tests substitute their own (Backend.start).
type process interface {
	pid() int
	// wait blocks until the process exits and returns its exit status, -1
	// when a signal ended it. It is called once. Output from descendants
	// that kept the pipe may still be arriving.
	wait() (int, error)
	// signalGroup signals the process's group. A group that is gone is not
	// an error.
	signalGroup(sig syscall.Signal) error
	// closeIO waits up to d for the output to reach EOF, then closes the
	// process's pipes.
	closeIO(d time.Duration)
}

// execProcess is a real process. Its stdio are pipes the backend owns and
// copies itself, so wait returns when the process exits instead of when the
// last descendant holding its output does: those are the proof's to end.
type execProcess struct {
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdinW     *os.File
	outputR    *os.File
	outputDone chan struct{}
}

// startExec starts spec in its own process group. procbound.Bind sets the
// group and, as a backstop, kills it when closeIO cancels the command's
// context.
func startExec(spec processSpec) (process, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, spec.argv[0], spec.argv[1:]...) //nolint:gosec // G204: sandbox-exec with the backend's own rule file and the configured program
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	procbound.Bind(cmd, 0)
	p := &execProcess{cmd: cmd, cancel: cancel, outputDone: make(chan struct{})}
	var childEnds []*os.File
	fail := func(err error) (process, error) {
		for _, f := range append(childEnds, p.stdinW, p.outputR) {
			if f != nil {
				_ = f.Close()
			}
		}
		cancel()
		return nil, err
	}
	if spec.stdin != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(fmt.Errorf("stdin pipe: %w", err))
		}
		cmd.Stdin, p.stdinW = r, w
		childEnds = append(childEnds, r)
	}
	if spec.output != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return fail(fmt.Errorf("output pipe: %w", err))
		}
		cmd.Stdout, cmd.Stderr, p.outputR = w, w, r
		childEnds = append(childEnds, w)
	} else {
		close(p.outputDone)
	}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	for _, f := range childEnds {
		_ = f.Close()
	}
	if spec.stdin != nil {
		go func() {
			// A write to a pipe nobody reads fails with EPIPE once the
			// readers are dead; Go doesn't die of SIGPIPE on it.
			_, _ = io.Copy(p.stdinW, spec.stdin)
			_ = p.stdinW.Close()
		}()
	}
	if spec.output != nil {
		go func() {
			defer close(p.outputDone)
			_, _ = io.Copy(spec.output, p.outputR)
		}()
	}
	return p, nil
}

func (p *execProcess) pid() int { return p.cmd.Process.Pid }

func (p *execProcess) wait() (int, error) {
	err := p.cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return -1, err
	}
	return p.cmd.ProcessState.ExitCode(), nil
}

func (p *execProcess) signalGroup(sig syscall.Signal) error {
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (p *execProcess) closeIO(d time.Duration) {
	if p.outputR != nil {
		select {
		case <-p.outputDone:
		case <-time.After(d):
		}
		_ = p.outputR.Close()
		<-p.outputDone
	}
	if p.stdinW != nil {
		_ = p.stdinW.Close()
	}
	p.cancel()
}
