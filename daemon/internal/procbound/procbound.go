// Package procbound binds a subprocess's lifetime so that neither
// cancellation nor a descendant it leaves behind can block the daemon
// indefinitely.
//
// os/exec builds a pipe whenever Stdout or Stderr is a writer that is not
// an *os.File, and Wait then blocks on the copier draining that pipe
// rather than on the process. A descendant that inherited the write end
// keeps Wait blocked long after the process itself is gone: git forking
// git-remote-https, the container CLI's helper, a background job a recipe
// step started. The daemon's own subprocess sites are unattended and
// supervised, so an unbounded Wait becomes a hung teardown, not a stuck
// terminal a human can notice.
//
// Binding sets three things together because none of them suffices alone:
//
//   - WaitDelay bounds the wait for those copiers, so Wait returns rather
//     than hanging;
//   - Setpgid puts the child in its own process group, so the descendants
//     that inherited the pipe are addressable as one;
//   - Cancel SIGKILLs that whole group, so cancellation reaches the
//     descendant holding the pipe and not only the direct child.
//
// WaitDelay unblocks the parent but reaps nothing, so Reap kills the group
// once the process is done and a background descendant does not outlive
// the call on the host.
//
// This is the pattern internal/verify/procroom.go established under a
// refute-first pass; the helper exists because five more call sites need
// it. Unix only, as the daemon already is.
package procbound

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// DefaultWaitDelay bounds the wait for a command's I/O pipes after its
// process exits or its context is canceled. It matches the bound the
// verify room and its git runner already chose independently.
const DefaultWaitDelay = 10 * time.Second

// Bind configures cmd's process-group and wait bounds. It must be called
// before the command starts, and cmd must have been created with
// exec.CommandContext: Start rejects a Cancel function on a Cmd built
// without a context.
//
// A waitDelay of zero or less selects DefaultWaitDelay. os/exec reads a
// zero WaitDelay as "wait forever", which is the failure this package
// exists to prevent, so an unbounded wait is deliberately not selectable
// here.
func Bind(cmd *exec.Cmd, waitDelay time.Duration) {
	if waitDelay <= 0 {
		waitDelay = DefaultWaitDelay
	}
	cmd.WaitDelay = waitDelay
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The group is already gone, so the command finished before
			// cancellation reached it. os/exec treats only os.ErrProcessDone
			// as a benign Cancel result and wraps anything else as
			// "exec: canceling Cmd: ...", which would turn a git or container
			// call that succeeded into a failure whenever a shutdown races
			// its exit.
			return os.ErrProcessDone
		}
		return err
	}
}

// Reap kills whatever remains of the command's process group. Call it
// once the process is done, after Run or Wait has returned, never before:
// it is the counterpart to WaitDelay, which unblocks the parent without
// terminating the descendant that blocked it.
//
// Best effort by design; ESRCH just means nothing lingered. Wait frees the
// leader's pid, but a process group cannot be recycled while it still has
// members, which is exactly the case worth reaping.
//
// Reap does nothing unless Bind put the child in its own group: without
// Setpgid the child sits in the daemon's process group, and the negated
// pid would name a group the daemon never created.
func Reap(cmd *exec.Cmd) {
	if cmd.Process == nil || cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// Run is Bind, cmd.Run, and Reap in the one order that is correct, for the
// callers that own the whole command. Callers that drive the process
// themselves, through StdoutPipe and Start/Wait, use Bind and Reap
// directly around their own sequence.
//
// The error is cmd.Run's, unchanged: exec.ErrWaitDelay reports that the
// process finished but its pipes outlived it, which callers classify like
// any other failed invocation.
func Run(cmd *exec.Cmd, waitDelay time.Duration) error {
	Bind(cmd, waitDelay)
	err := cmd.Run()
	Reap(cmd)
	return err
}
