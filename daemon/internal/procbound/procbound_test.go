package procbound

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunReturnsWhileDescendantHoldsPipe is the bug this package exists
// for: the command exits, a background descendant it left behind still
// holds the inherited stdout pipe, and os/exec's copier keeps Wait
// blocked on the pipe rather than on the process.
func TestRunReturnsWhileDescendantHoldsPipe(t *testing.T) {
	var out strings.Builder
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 30 & echo done")
	cmd.Stdout = &out
	start := time.Now()
	err := Run(cmd, 200*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Run blocked %v on a descendant-held pipe", elapsed)
	}
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run err = %v, want exec.ErrWaitDelay so callers can classify the unclosed pipe", err)
	}
	if got := strings.TrimSpace(out.String()); got != "done" {
		t.Fatalf("stdout = %q, want the output written before the delay elapsed", got)
	}
}

// TestRunReapsDescendant pins the half WaitDelay does not do: unblocking
// the parent leaves the descendant running on the host, so Run kills the
// process group once the command is done.
func TestRunReapsDescendant(t *testing.T) {
	var out strings.Builder
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 30 & echo $!")
	cmd.Stdout = &out
	if err := Run(cmd, 200*time.Millisecond); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run err = %v, want exec.ErrWaitDelay", err)
	}
	requireExited(t, parsePID(t, out.String()))
}

// TestRunLeavesACleanCommandAlone guards the other direction: binding a
// command must not turn an ordinary successful invocation into a failure.
func TestRunLeavesACleanCommandAlone(t *testing.T) {
	var out strings.Builder
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "echo done")
	cmd.Stdout = &out
	if err := Run(cmd, 200*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "done" {
		t.Fatalf("stdout = %q, want %q", got, "done")
	}
}

// TestCancelReportsAVanishedGroupAsDone guards a misclassification, not a
// hang: os/exec treats only os.ErrProcessDone as a benign Cancel result and
// wraps anything else as "exec: canceling Cmd: ...", which Wait then
// returns. So when cancellation races a command's own exit — a daemon
// shutdown arriving just as a git call finishes — a raw ESRCH from the
// group kill would report the successful call as failed.
//
// Reaping the command first puts Cancel in exactly that state
// deterministically, without having to win a race.
func TestCancelReportsAVanishedGroupAsDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 0")
	var out strings.Builder
	cmd.Stdout = &out
	if err := Run(cmd, 200*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Run has reaped the process and Reap has killed the group, so the
	// group the callback names no longer exists.
	err := cmd.Cancel()
	if errors.Is(err, syscall.ESRCH) {
		t.Fatal("Cancel returned a raw ESRCH; os/exec turns that into " +
			"\"exec: canceling Cmd: no such process\" on a command that succeeded")
	}
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel err = %v, want nil or os.ErrProcessDone", err)
	}
}

// TestBindCancelKillsTheDescendantHoldingThePipe covers the callers that
// drive the process themselves through StdoutPipe: their read blocks
// before Wait is ever called, so WaitDelay's timer has not started and
// only the process-group kill can unblock them. The delay here is long
// enough that a pipe closed by WaitDelay expiry would fail the deadline.
func TestBindCancelKillsTheDescendantHoldingThePipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!")
	Bind(cmd, 30*time.Second)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(stdout)
		read <- b
	}()
	// The descendant holds the write end, so the read cannot reach EOF
	// until cancellation kills the whole group.
	select {
	case b := <-read:
		t.Fatalf("read returned %q before cancellation; the descendant did not hold the pipe", b)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	var buf []byte
	select {
	case buf = <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("read stayed blocked after cancellation")
	}
	_ = cmd.Wait()
	Reap(cmd)
	requireExited(t, parsePID(t, string(buf)))
}

func parsePID(t *testing.T, out string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("parse descendant pid from %q: %v", out, err)
	}
	return pid
}

// requireExited polls with signal 0, which probes existence without
// delivering anything; ESRCH means the descendant is gone.
func requireExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("descendant %d still alive after the command completed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
