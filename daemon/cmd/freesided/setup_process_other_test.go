//go:build !linux

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// processCommandLine returns pid's arguments as ps reports them.
func processCommandLine(pid int) (string, error) {
	out, err := exec.Command( //nolint:gosec // G204: ps is fixed and the PID is the caller's child process.
		"ps", "-ww", "-o", "command=", "-p", fmt.Sprint(pid),
	).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
