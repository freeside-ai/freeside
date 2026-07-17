package verify

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newProcRoom(t *testing.T) *ProcRoom {
	t.Helper()
	return &ProcRoom{Home: t.TempDir()}
}

func TestProcRoomExitCodes(t *testing.T) {
	r := newProcRoom(t)
	pass, err := r.Run(context.Background(), t.TempDir(), []string{"true"})
	if err != nil || pass.ExitCode != 0 {
		t.Fatalf("true: result %+v, err %v", pass, err)
	}
	fail, err := r.Run(context.Background(), t.TempDir(), []string{"false"})
	if err != nil {
		t.Fatalf("false: %v", err)
	}
	if fail.ExitCode == 0 {
		t.Fatal("false reported exit 0")
	}
}

func TestProcRoomCapturesAndBoundsOutput(t *testing.T) {
	r := newProcRoom(t)
	res, err := r.Run(context.Background(), t.TempDir(), []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if !bytes.Equal(res.Output, []byte("hello\n")) || res.Truncated {
		t.Fatalf("echo result %+v, want full %q", res, "hello\n")
	}
	r.MaxOutputBytes = 4
	res, err = r.Run(context.Background(), t.TempDir(), []string{"echo", "hello world"})
	if err != nil {
		t.Fatalf("bounded echo: %v", err)
	}
	if !bytes.Equal(res.Output, []byte("hell")) || !res.Truncated {
		t.Fatalf("bounded result %+v, want 4 bytes and Truncated", res)
	}
}

// TestProcRoomScrubsEnvironment is the credential-leak guarantee: a
// secret planted in the daemon's environment is invisible to the child,
// and the child's HOME is the scratch, never the real one.
func TestProcRoomScrubsEnvironment(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "planted-credential")
	t.Setenv("GITHUB_TOKEN", "planted-token")
	r := newProcRoom(t)
	res, err := r.Run(context.Background(), t.TempDir(), []string{"env"})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	out := string(res.Output)
	for _, leaked := range []string{"planted-credential", "planted-token", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN"} {
		if strings.Contains(out, leaked) {
			t.Errorf("child environment leaks %s", leaked)
		}
	}
	if !strings.Contains(out, "HOME="+r.Home) {
		t.Error("child HOME is not the scratch home")
	}
}

func TestProcRoomTimeoutIsAFailedStep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := newProcRoom(t)
	res, err := r.Run(ctx, t.TempDir(), []string{"sleep", "10"})
	if err != nil {
		t.Fatalf("timed-out sleep returned a room error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("timed-out sleep reported exit 0")
	}
}

func TestProcRoomFailsLoud(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		r := newProcRoom(t)
		if _, err := r.Run(context.Background(), t.TempDir(), []string{"freeside-no-such-binary"}); err == nil {
			t.Fatal("missing binary did not error")
		}
	})
	t.Run("unset home", func(t *testing.T) {
		r := &ProcRoom{}
		if _, err := r.Run(context.Background(), t.TempDir(), []string{"true"}); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("err = %v, want ErrInvalidOptions", err)
		}
	})
	t.Run("empty argv", func(t *testing.T) {
		r := newProcRoom(t)
		if _, err := r.Run(context.Background(), t.TempDir(), nil); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("err = %v, want ErrInvalidOptions", err)
		}
	})
	t.Run("negative cap", func(t *testing.T) {
		r := newProcRoom(t)
		r.MaxOutputBytes = -1
		if _, err := r.Run(context.Background(), t.TempDir(), []string{"true"}); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("err = %v, want ErrInvalidOptions", err)
		}
	})
}

func TestProcRoomRecordsInvocations(t *testing.T) {
	r := newProcRoom(t)
	dir := t.TempDir()
	if _, err := r.Run(context.Background(), dir, []string{"true"}); err != nil {
		t.Fatalf("true: %v", err)
	}
	if _, err := r.Run(context.Background(), dir, []string{"echo", "x"}); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if len(r.Invocations) != 2 {
		t.Fatalf("recorded %d invocations, want 2", len(r.Invocations))
	}
	if r.Invocations[1].Argv[0] != "echo" || r.Invocations[1].Dir != dir {
		t.Fatalf("second invocation = %+v", r.Invocations[1])
	}
}

// TestProcRoomWaitDelayUnblocksHeldPipe is the refute-pass regression:
// a command that exits while a grandchild it spawned still holds the
// output pipe must not block Run past the wait delay, and must read as
// a failed step, never a passed one.
func TestProcRoomWaitDelayUnblocksHeldPipe(t *testing.T) {
	r := newProcRoom(t)
	r.WaitDelay = 200 * time.Millisecond
	start := time.Now()
	res, err := r.Run(context.Background(), t.TempDir(), []string{"sh", "-c", "sleep 5 & echo done"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Run blocked %v on a grandchild-held pipe", elapsed)
	}
	if res.ExitCode == 0 {
		t.Fatal("a step with a lingering pipe-holding descendant read as passed")
	}
}

// TestProcRoomReapsDescendants is the Codex-review regression: a
// background process the command leaves behind must not outlive the
// step on the host; the room kills the whole process group before
// reporting the step complete.
func TestProcRoomReapsDescendants(t *testing.T) {
	r := newProcRoom(t)
	r.WaitDelay = 200 * time.Millisecond
	res, err := r.Run(context.Background(), t.TempDir(), []string{"sh", "-c", "sleep 30 & echo $!"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(res.Output)))
	if err != nil {
		t.Fatalf("parse descendant pid from %q: %v", res.Output, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		// Signal 0 probes existence; ESRCH means the descendant is gone.
		if killErr := syscall.Kill(pid, 0); errors.Is(killErr, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("descendant %d still alive after the step completed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
