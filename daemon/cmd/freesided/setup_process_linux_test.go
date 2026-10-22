//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// processCommandLine returns pid's arguments joined by spaces. It reads
// /proc rather than running ps, which a minimal project image lacks. The
// kernel releases a vforked parent before the child's new image records
// its arguments, so a just-started child briefly reads as empty; the
// helper rereads until they appear or a deadline passes.
func processCommandLine(pid int) (string, error) {
	path := fmt.Sprintf("/proc/%d/cmdline", pid)
	deadline := time.Now().Add(5 * time.Second)
	for {
		body, err := os.ReadFile(path) //nolint:gosec // G703: the path is fixed apart from the caller's child PID.
		if err != nil {
			return "", err
		}
		if len(body) > 0 || time.Now().After(deadline) {
			return strings.Join(strings.Split(strings.TrimSuffix(string(body), "\x00"), "\x00"), " "), nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
