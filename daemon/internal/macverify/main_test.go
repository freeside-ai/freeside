package macverify

import (
	"os"
	"testing"
)

// TestMain answers the probe command the way freesided does, because the
// room stages this test binary as the probe: a live room run copies
// os.Executable() into its probe workspace and runs it as a slot.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == probeCommand[1] && os.Args[2] == probeCommand[2] {
		os.Exit(RunProbe(os.Args[3:], os.Stdout))
	}
	os.Exit(m.Run())
}
