package main

import (
	"errors"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
)

// The daemon takes each run's base from the tip it observes, so it no longer
// accepts a configured base: a launcher still passing -base-sha fails at
// flag parsing instead of running with a base it ignores. The daemon is this
// test binary re-executed as freesided's main (runKillTestDaemon).
func TestFreesidedRejectsTheConfiguredBaseFlag(t *testing.T) {
	t.Parallel()
	cmd := osexec.Command(os.Args[0], //nolint:gosec // G204: execute this package's test binary with fixed arguments
		"-test.run=^TestDaemonRecoversAcrossSIGKILL$", "--",
		"-base-sha", strings.Repeat("a", 40))
	cmd.Env = append(os.Environ(), killTestDaemonEnv+"=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *osexec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("freesided -base-sha = %v, want exit status 2; stderr:\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -base-sha") {
		t.Fatalf("stderr does not name the removed flag:\n%s", stderr.String())
	}
}
