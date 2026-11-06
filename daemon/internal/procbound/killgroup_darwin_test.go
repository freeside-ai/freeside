//go:build darwin

package procbound

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

// SRUN in darwin's <sys/proc.h>.
const stateRunning = 2

func TestOnlyZombies(t *testing.T) {
	cases := []struct {
		name   string
		states []int8
		want   bool
	}{
		{"zombie leader only", []int8{stateZombie}, true},
		{"no members left", []int8{}, true},
		{"zombie leader and a running member", []int8{stateZombie, stateRunning}, false},
		{"running member only", []int8{stateRunning}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := onlyZombies(c.states); got != c.want {
				t.Fatalf("onlyZombies(%v) = %v, want %v", c.states, got, c.want)
			}
		})
	}
}

// TestBindCancelReportsAZombieOnlyGroupAsDone guards the darwin window
// between a command's exit and its reap. There the group's only member is
// the zombie leader, and darwin refuses the group kill with EPERM, not
// ESRCH, so a cancellation landing then would wrap EPERM as "exec:
// canceling Cmd: operation not permitted" on a command that succeeded.
//
// Holding off Wait until the leader is a zombie puts the callback in that
// state deterministically.
func TestBindCancelReportsAZombieOnlyGroupAsDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	Bind(cmd, 0)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitZombie(t, cmd.Process.Pid)

	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel on a zombie-only group = %v, want os.ErrProcessDone", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// TestGroupOnlyZombiesSeesALiveMember shows the lookup is what keeps a
// live member from reading as done: with the leader a zombie and a
// background sleep still in its group, the group is not zombie-only.
func TestGroupOnlyZombiesSeesALiveMember(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	awaitZombie(t, pgid)

	done, err := groupOnlyZombies(pgid)
	if err != nil {
		t.Fatalf("groupOnlyZombies: %v", err)
	}
	if done {
		t.Fatal("groupOnlyZombies reported a group with a live sleep as zombie-only")
	}
}

// awaitZombie polls until pid has exited and awaits its reap. The code
// under test uses no timers, so a bounded real-clock poll is enough.
func awaitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			t.Fatalf("kern.proc.pid %d: %v", pid, err)
		}
		if p.Proc.P_stat == stateZombie {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d did not become a zombie within 5s (p_stat %d)", pid, p.Proc.P_stat)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
