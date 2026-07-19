package export_test

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// TestHelperInterfacePinned pins the fixed helper-in-image interface both
// consuming lanes bind to (plan §5.6): the contracted binary path, the mount
// points, and the daemon-owned fixed command. A change here is an interface
// change for ward's exporter image and gauntlet's import, not a refactor.
func TestHelperInterfacePinned(t *testing.T) {
	if export.HelperPath != "/usr/local/bin/freeside-export" {
		t.Errorf("HelperPath = %q", export.HelperPath)
	}
	if export.HelperWorkspaceDir != "/workspace" {
		t.Errorf("HelperWorkspaceDir = %q", export.HelperWorkspaceDir)
	}
	if export.HelperHandoffDir != "/handoff" {
		t.Errorf("HelperHandoffDir = %q", export.HelperHandoffDir)
	}
	want := []string{
		"/usr/local/bin/freeside-export",
		"-workspace", "/workspace",
		"-out", "/handoff",
	}
	if got := export.HelperCommand(); !slices.Equal(got, want) {
		t.Errorf("HelperCommand() = %q, want %q", got, want)
	}
}

// TestHelperCommandFresh proves each call returns its own slice: a caller
// mutating its copy of the fixed command cannot corrupt another's.
func TestHelperCommandFresh(t *testing.T) {
	a := export.HelperCommand()
	b := export.HelperCommand()
	a[0] = "/tmp/evil"
	if b[0] != export.HelperPath {
		t.Fatal("HelperCommand shares backing storage between calls")
	}
}
