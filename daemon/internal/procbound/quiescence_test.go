package procbound

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestConfirmExitRequiresJoinedOwnedGroup(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	if err := Run(cmd, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmExit(cmd, time.Second); err != nil {
		t.Fatal(err)
	}
	state := cmd.ProcessState
	cmd.ProcessState = nil
	if err := ConfirmExit(cmd, time.Second); !errors.Is(err, ErrQuiescenceUnproven) {
		t.Fatal("unjoined command confirmed")
	}
	cmd.ProcessState = state
	cmd.SysProcAttr.Setpgid = false
	if err := ConfirmExit(cmd, time.Second); !errors.Is(err, ErrQuiescenceUnproven) {
		t.Fatal("unowned group confirmed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	neverStarted := exec.CommandContext(ctx, "sh", "-c", "exit 0")
	if err := Run(neverStarted, time.Second); err == nil {
		t.Fatal("cancelled command launched")
	}
	if err := ConfirmExit(neverStarted, time.Second); err != nil {
		t.Fatalf("no-launch proof: %v", err)
	}
}

func TestGroupAbsenceCannotBeInferredFromTimeoutOrProbeFailure(t *testing.T) {
	for _, probeErr := range []error{nil, syscall.EPERM, syscall.ESRCH} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := awaitGroupAbsence(ctx, func() error { return probeErr })
			if errors.Is(probeErr, syscall.ESRCH) {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrQuiescenceUnproven) {
				t.Fatalf("surviving or unobservable group confirmed: %v", err)
			}
		})
	}
}
