package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	killTestCheckpointEnv = "FREESIDE_KILL_TEST_CHECKPOINT"
	killTestMarkerEnv     = "FREESIDE_KILL_TEST_MARKER"
)

// markKillTestCheckpoint is the kill-recovery daemon's checkpoint hook. At
// the checkpoint the test names, it publishes the marker file atomically and
// blocks forever, so the test SIGKILLs a daemon stopped at a known point.
func markKillTestCheckpoint(name string) error {
	target := os.Getenv(killTestCheckpointEnv)
	if target == "" || target != name {
		return nil
	}
	marker := os.Getenv(killTestMarkerEnv)
	if marker == "" {
		return errors.New("kill-test checkpoint marker is not configured")
	}
	file, err := os.CreateTemp(filepath.Dir(marker), ".freeside-kill-checkpoint-*")
	if err != nil {
		return fmt.Errorf("create kill-test checkpoint %q: %w", name, err)
	}
	temp := file.Name()
	defer func() { _ = os.Remove(temp) }() //nolint:gosec // G703: temp file beside the test-set marker path
	if _, err := file.WriteString(name); err != nil {
		_ = file.Close()
		return fmt.Errorf("write kill-test checkpoint %q: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close kill-test checkpoint %q: %w", name, err)
	}
	if err := os.Rename(temp, marker); err != nil { //nolint:gosec // G703: the parent test sets the marker path
		return fmt.Errorf("publish kill-test checkpoint %q: %w", name, err)
	}
	select {}
}
