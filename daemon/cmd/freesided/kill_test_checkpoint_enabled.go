//go:build freeside_kill_test

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func killTestCheckpoint(name string) error {
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
	defer func() { _ = os.Remove(temp) }()
	if _, err := file.WriteString(name); err != nil {
		_ = file.Close()
		return fmt.Errorf("write kill-test checkpoint %q: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close kill-test checkpoint %q: %w", name, err)
	}
	if err := os.Rename(temp, marker); err != nil {
		return fmt.Errorf("publish kill-test checkpoint %q: %w", name, err)
	}
	select {}
}
