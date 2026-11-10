//go:build darwin

package macverify

import (
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// TestLiveSuiteProvesTheExecutor runs the conformance pass against the
// installed executor, with the packet-filter rules the installer loads, and
// checks it records a pass and declares. The pass's probe is this test
// binary, which answers the probe verb (TestMain). Set
// FREESIDE_MACVERIFY_OFF_HOST_UDP to an IP:port DNS server when the first
// off-host nameserver in /etc/resolv.conf does not answer.
//
//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLiveSuiteProvesTheExecutor(t *testing.T) {
	room, _ := liveRoom(t)
	rec := &memRecorder{}
	s, err := NewSuite(room.backend, SuiteFixture{OffHostUDP: os.Getenv("FREESIDE_MACVERIFY_OFF_HOST_UDP")},
		WithConformanceRecorder(rec))
	if err != nil {
		t.Fatalf("NewSuite: %v", err)
	}
	if err := s.Full(t.Context()); err != nil {
		t.Fatalf("Full: %v", err)
	}
	if got := rec.outcomes(); !slices.Equal(got, []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformancePassed}) {
		t.Fatalf("records = %v, want superseded then passed", got)
	}
	if !room.backend.Capabilities().Has(exec.CapCredentialFreeHostVerification) {
		t.Fatal("a recorded passing pass did not declare")
	}
}

// TestLivePreJobProbeRefusesAPermissiveProfile configures the executor with
// a profile that confines nothing: the pre-job probe must refuse the command
// before it runs, naming the sandbox network confinement.
//
//nolint:paralleltest // Serial: each live test builds its own Backend, and Backends do not share leases, so parallel tests could lease one slot twice.
func TestLivePreJobProbeRefusesAPermissiveProfile(t *testing.T) {
	_, cfg := liveRoom(t)
	cfg.SandboxProfile = permissiveProfile
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	room, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	res, err := room.Run(t.Context(), probeWorkspace(t), []string{"./probe", "confine"})
	var failure *ConformanceFailure
	if !errors.As(err, &failure) || failure.Confinement != ConfinementSandboxNetwork {
		t.Fatalf("Run = exit %d, %v; want a sandbox_network failure\n%s", res.ExitCode, err, res.Output)
	}
	if res.Output != nil {
		t.Fatalf("the command ran after a failed pre-job probe:\n%s", res.Output)
	}
	t.Logf("refused as expected: %v", err)
}
