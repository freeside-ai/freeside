// Package execfixture writes test-owned files that a test then executes,
// such as fake binaries and stub scripts, without the ETXTBSY race in
// go.dev/issue/22315.
//
// While a test writes a script, a fork elsewhere in the same test binary
// (a parallel test running exec.Command) inherits the open write
// descriptor until that child execs. An exec of the script in that window
// fails with "text file busy". Go's fork path holds syscall.ForkLock for
// writing, so Write holds it for reading across the whole write,
// including Close: no fork can start while the descriptor is open.
//
// The lock covers only forks made through the syscall package. A fork
// from cgo or a non-Go thread is not covered; the daemon's tests make
// none. Rationale: devlog/2026-09-04-1502-publish-exec-diagnostics.md.
//
// Usage (from any _test.go):
//
//	execfixture.Write(t, path, []byte("#!/bin/sh\nexit 0\n"), 0o700)
//
// A fixture that a test writes but never executes needs none of this.
package execfixture

import (
	"os"
	"syscall"
	"testing"
)

// Write writes body to path with mode, as os.WriteFile does (so the
// umask applies), while no fork can inherit the write descriptor. It
// fails the test on error.
func Write(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, body, mode)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}
