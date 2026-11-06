//go:build darwin

package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestProcRoomCancelReportsAZombieOnlyGroupAsDone extends
// TestProcRoomCancelReportsAVanishedGroupAsDone to the darwin window
// before the reap. While the exited leader is a zombie, darwin refuses the
// group kill with EPERM, not ESRCH, so a cancellation landing then would
// turn a step that passed into "room could not execute ...: exec:
// canceling Cmd: operation not permitted".
//
// Holding off Wait until the leader is a zombie puts the callback in that
// state deterministically.
func TestProcRoomCancelReportsAZombieOnlyGroupAsDone(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(context.Background(), "true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = cancelProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid

	// SZOMB in darwin's <sys/proc.h>. The code under test uses no timers,
	// so a bounded real-clock poll is enough.
	const stateZombie = 5
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			t.Fatalf("kern.proc.pid %d: %v", pid, err)
		}
		if p.Proc.P_stat == stateZombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d did not become a zombie within 5s (p_stat %d)", pid, p.Proc.P_stat)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel on a zombie-only group = %v, want os.ErrProcessDone", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
