//go:build darwin && cgo

package cordon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunProbeUnconfinedFailsEveryCheck shows the probe can't pass outside
// a launch: every check fails there, so a pass is the rules' doing. A
// refusal for any reason but the sandbox's fails too.
func TestRunProbeUnconfinedFailsEveryCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	standIn := exec.Command("/bin/sleep", "600")
	if err := standIn.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = standIn.Process.Kill(); _ = standIn.Wait() })
	start, err := processStart(standIn.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	// check.sh runs the tests under a sandbox of its own, which may deny
	// this process an operation already.
	var ops []string
	if denied, err := sandboxDeniesOperation("job-creation"); err != nil {
		t.Fatal(err)
	} else if !denied {
		ops = append(ops, "job-creation")
	}
	spec := probeSpec{
		Reads:    []probeRead{{Path: present, Present: true}, {Path: filepath.Join(dir, "absent")}},
		Creates:  []probeCreate{{Path: filepath.Join(dir, "new")}, {Path: filepath.Join(dir, "newdir"), Dir: true}},
		Sockets:  []string{filepath.Join(dir, "nothing.sock")},
		Ports:    []int{freePort(t)},
		Services: []probeService{{Name: "com.apple.cfprefsd.daemon", Registered: true}},
		Denied:   ops,
		StandIn:  &probeStandIn{Pid: standIn.Process.Pid, Start: start},
		Commands: []probeCommand{{Name: "true", Argv: []string{"/usr/bin/true"}}},
	}
	var out strings.Builder
	if code := RunProbe([]string{spec.arg()}, &out); code != 1 {
		t.Fatalf("RunProbe = %d, want 1:\n%s", code, out.String())
	}
	for _, check := range spec.checks() {
		if check == checkRead(filepath.Join(dir, "absent")) {
			// Nothing to read anywhere, which passes; the suite's control
			// is what found it absent.
			continue
		}
		if !strings.Contains(out.String(), "FAIL "+check+": ") {
			t.Errorf("%s did not fail unconfined:\n%s", check, out.String())
		}
	}
	// The exclusive creates removed what they made.
	for _, c := range spec.Creates {
		if _, err := os.Lstat(c.Path); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", c.Path, err)
		}
	}
	if _, err := checkProbeReport(spec, []byte(out.String()), 1); err == nil {
		t.Fatal("an unconfined report passed")
	}
}

// TestRunProbeSignalsOnlyItsStandIn shows a pid whose start time no longer
// matches is never signaled.
func TestRunProbeSignalsOnlyItsStandIn(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	spec := probeSpec{StandIn: &probeStandIn{Pid: os.Getpid(), Start: 1}}
	if code := RunProbe([]string{spec.arg()}, &out); code != 1 ||
		!strings.Contains(out.String(), "is not the suite's stand-in") {
		t.Fatalf("RunProbe = %d:\n%s", code, out.String())
	}
}
