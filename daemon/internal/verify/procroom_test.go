package verify

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
