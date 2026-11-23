package cordon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
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
	// secret, when set, is the read end of a pipe holding the secret,
	// whose write end is already closed (secretPipe): the process gets it
	// on tokenFD, so its read reaches end of file. The caller keeps and
	// closes its own copy.
	secret *os.File
	// stdout and stderr receive the process's two output streams. Nil
	// sends a stream to /dev/null.
	stdout, stderr io.Writer
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
	// closeIO waits up to d for both output streams to reach EOF, then
	// closes the process's pipes.
	closeIO(d time.Duration)
}

// execProcess is a real process. Its stdio are pipes the backend owns and
// copies itself, so wait returns when the process exits instead of when the
// last descendant holding its output does: those are the proof's to end.
type execProcess struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stdinW *os.File
	// outputs are the read ends of the stdout and stderr pipes the
	// backend copies; outputDone closes once every copy has finished.
	outputs    []*os.File
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
		for _, f := range append(append(childEnds, p.stdinW), p.outputs...) {
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
	if spec.secret != nil {
		cmd.ExtraFiles = []*os.File{spec.secret}
	}
	var sinks []io.Writer
	for _, stream := range []struct {
		name string
		sink io.Writer
		set  func(io.Writer)
	}{
		{"stdout", spec.stdout, func(w io.Writer) { cmd.Stdout = w }},
		{"stderr", spec.stderr, func(w io.Writer) { cmd.Stderr = w }},
	} {
		if stream.sink == nil {
			continue
		}
		r, w, err := os.Pipe()
		if err != nil {
			return fail(fmt.Errorf("%s pipe: %w", stream.name, err))
		}
		stream.set(w)
		childEnds = append(childEnds, w)
		p.outputs, sinks = append(p.outputs, r), append(sinks, stream.sink)
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
	var copies sync.WaitGroup
	for i, r := range p.outputs {
		copies.Go(func() { _, _ = io.Copy(sinks[i], r) })
	}
	go func() {
		copies.Wait()
		close(p.outputDone)
	}()
	return p, nil
}

// secretPipe returns the read end of a pipe that holds secret and whose
// write end is already closed. Writing before the reader starts can't
// block: a setup token is at most ward.MaxSetupTokenBytes, 4096, and a
// macOS pipe buffers at least 16 KiB. os.Pipe sets close-on-exec on both
// ends, so only the process that gets the read end in ExtraFiles inherits
// it.
func secretPipe(secret []byte) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("secret pipe: %w", err)
	}
	_, err = w.Write(secret)
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("write secret pipe: %w", err)
	}
	return r, nil
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
	if len(p.outputs) > 0 {
		select {
		case <-p.outputDone:
		case <-time.After(d):
		}
		for _, r := range p.outputs {
			_ = r.Close()
		}
	}
	<-p.outputDone
	if p.stdinW != nil {
		_ = p.stdinW.Close()
	}
	p.cancel()
}
