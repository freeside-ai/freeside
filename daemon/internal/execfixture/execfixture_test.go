package execfixture

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteSurvivesParallelForks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux enforces ETXTBSY for these scripts")
	}
	t.Parallel()
	var stop atomic.Bool
	var forks sync.WaitGroup
	for range 16 {
		forks.Go(func() {
			for !stop.Load() {
				if err := exec.CommandContext(t.Context(), "/bin/true").Run(); err != nil {
					t.Errorf("parallel fork: %v", err)
					return
				}
			}
		})
	}
	defer func() {
		stop.Store(true)
		forks.Wait()
	}()
	path := filepath.Join(t.TempDir(), "script.sh")
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		Write(t, path, []byte("#!/bin/sh\nexit 0\n"), 0o700)
		if err := exec.CommandContext(t.Context(), path).Run(); err != nil { //nolint:gosec // G204: test-owned executable
			t.Fatal(err)
		}
	}
}
