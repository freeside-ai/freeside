package main

import (
	"flag"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
)

func parseCordonFlags(t *testing.T, args ...string) daemoncompose.CordonBackendConfig {
	t.Helper()
	flags := flag.NewFlagSet("freesided", flag.ContinueOnError)
	config := cordonBackendFlags(flags)
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	return config()
}

func TestCordonFlagsUnsetConfigureNone(t *testing.T) {
	t.Parallel()
	got := parseCordonFlags(t)
	if got.AgentCLI != "" {
		t.Errorf("no flags configure a cordon: %+v", got)
	}
	if got.StallLimit != 10*time.Minute || got.EndGrace != 10*time.Second || got.EndProofTimeout != time.Minute {
		t.Errorf("default durations = %v, %v, %v", got.StallLimit, got.EndGrace, got.EndProofTimeout)
	}
}

func TestCordonFlagsConfigureTheBackend(t *testing.T) {
	t.Parallel()
	got := parseCordonFlags(t, "-cordon-agent-cli", "/opt/claude", "-cordon-agent-cli-sha256", "abc",
		"-cordon-projects", "/p", "-cordon-protected-root", "/a", "-cordon-protected-root", "/b",
		"-cordon-stall-limit", "1s")
	if got.AgentCLI != "/opt/claude" || got.AgentCLISHA256 != "abc" || got.Projects != "/p" ||
		!slices.Equal(got.ProtectedRoots, []string{"/a", "/b"}) || got.StallLimit != time.Second {
		t.Errorf("flags configure %+v", got)
	}
}
