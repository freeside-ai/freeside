package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// hostB is a second execution host beside the daemon's.
const hostB domain.ExecutionHostID = "host-b"

func addHostB(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.AddExecutionHost(context.Background(), extraExecutionHost(hostB, "studio"))
	}); err != nil {
		t.Fatalf("add host B: %v", err)
	}
}

func recordConformanceOn(
	t *testing.T, s *store.Store, host domain.ExecutionHostID, record domain.BackendConformance,
) uint64 {
	t.Helper()
	var generation uint64
	if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		var err error
		generation, err = tx.RecordBackendConformance(context.Background(), host, record)
		return err
	}); err != nil {
		t.Fatalf("RecordBackendConformance on %s: %v", host, err)
	}
	return generation
}

func latestConformanceOn(
	t *testing.T, s *store.Store, host domain.ExecutionHostID,
) domain.BackendConformance {
	t.Helper()
	var (
		record domain.BackendConformance
		found  bool
	)
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		record, found, err = tx.LatestBackendConformance(
			context.Background(), host, domain.BackendFreshVMReadOnlyVolumeHandoff)
		return err
	}); err != nil {
		t.Fatalf("LatestBackendConformance on %s: %v", host, err)
	}
	if !found {
		t.Fatalf("no conformance record on %s", host)
	}
	return record
}

// TestConformanceAdmitsOnlyOnItsHost is #2165's acceptance 1 and the
// admission half of 2: both hosts share one configuration digest, a pass on
// host B admits nothing on the daemon's host, and once the daemon's host
// holds its own pass, host B's later failure and recheck refuse nothing
// there.
func TestConformanceAdmitsOnlyOnItsHost(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	addHostB(t, s)
	caps := conformantCapabilities(t)

	recordConformanceOn(t, s, hostB, conformanceAt(t, domain.ConformancePassed, caps, admissionEpoch))
	if err := requireConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("mint on another host's pass = %v, want %v", err, store.ErrBackendNotConformant)
	}
	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate on another host's pass = %v, want %v", err, store.ErrBackendNotConformant)
	}

	recordConformanceOn(t, s, domain.DaemonHostID,
		conformanceAt(t, domain.ConformancePassed, caps, admissionEpoch.Add(time.Minute)))
	recordConformanceOn(t, s, hostB, conformanceAt(t, domain.ConformanceFailed, nil, admissionEpoch.Add(2*time.Minute)))
	recordConformanceOn(t, s, hostB,
		conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(3*time.Minute)))
	if err := requireConformance(t, s, f.admission); err != nil {
		t.Fatalf("mint on the daemon's pass after host B failed = %v, want nil", err)
	}
	if err := authenticateConformance(t, s, f.admission); err != nil {
		t.Fatalf("authenticate on the daemon's pass after host B failed = %v, want nil", err)
	}
}

// TestRecheckSupersedesOnlyItsHost is acceptance 2's record half: a recheck
// marker on host B becomes B's declaration and leaves the daemon's host's
// declaration, and the host each read stamps, as they were.
func TestRecheckSupersedesOnlyItsHost(t *testing.T) {
	t.Parallel()
	s, _ := openUnattendedNoConformance(t)
	addHostB(t, s)
	caps := conformantCapabilities(t)

	daemonPass := recordConformanceOn(t, s, domain.DaemonHostID,
		conformanceAt(t, domain.ConformancePassed, caps, admissionEpoch))
	recordConformanceOn(t, s, hostB, conformanceAt(t, domain.ConformancePassed, caps, admissionEpoch))
	marker := recordConformanceOn(t, s, hostB,
		conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(time.Minute)))

	onB := latestConformanceOn(t, s, hostB)
	if onB.Outcome != domain.ConformanceSuperseded || onB.Generation != marker || onB.Host != hostB {
		t.Fatalf("host B's declaration = %+v, want the marker at generation %d on %s", onB, marker, hostB)
	}
	onDaemon := latestConformanceOn(t, s, domain.DaemonHostID)
	if onDaemon.Outcome != domain.ConformancePassed || onDaemon.Generation != daemonPass ||
		onDaemon.Host != domain.DaemonHostID {
		t.Fatalf("the daemon's declaration = %+v, want the pass at generation %d", onDaemon, daemonPass)
	}
}

// TestRecordBackendConformanceHostRefusals is acceptance 7: the store takes
// no record for a host it doesn't hold, and none that already names a host.
func TestRecordBackendConformanceHostRefusals(t *testing.T) {
	t.Parallel()
	s, _ := openUnattendedNoConformance(t)
	record := func(host domain.ExecutionHostID, c domain.BackendConformance) error {
		return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
			_, err := tx.RecordBackendConformance(context.Background(), host, c)
			return err
		})
	}
	pass := conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch)

	if err := record(hostB, pass); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("record on an unknown host = %v, want %v", err, store.ErrNotFound)
	}
	named := pass
	named.Host = domain.DaemonHostID
	if err := record(domain.DaemonHostID, named); !errors.Is(err, store.ErrConformanceHostSupplied) {
		t.Errorf("record naming a host = %v, want %v", err, store.ErrConformanceHostSupplied)
	}
}

// TestHostVerificationAdmitsOnlyOnItsHost is acceptance 1 for the host
// verification gate, which reads the newest record rather than one
// configuration's: host B's pass admits nothing on the daemon's host, and
// host B's later failure refuses nothing there.
func TestHostVerificationAdmitsOnlyOnItsHost(t *testing.T) {
	t.Parallel()
	const bound = domain.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	declared := domain.NewCapabilitySnapshot(domain.CapCredentialFreeHostVerification)
	s, _ := openUnattendedNoConformance(t)
	addHostB(t, s)
	hostRecord := func(outcome domain.ConformanceOutcome, caps domain.CapabilitySnapshot, at time.Time) domain.BackendConformance {
		t.Helper()
		record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
			Backend: domain.BackendCredentialFreeHostVerification, Outcome: outcome,
			ConfigurationDigest: bound, Capabilities: caps, ProvedAt: at,
		})
		if err != nil {
			t.Fatalf("NewBackendConformance: %v", err)
		}
		return record
	}
	require := func() error {
		return s.Read(context.Background(), func(tx *store.ReadTx) error {
			return tx.RequireHostVerificationConformant(context.Background(), bound, declared)
		})
	}

	recordConformanceOn(t, s, hostB, hostRecord(domain.ConformancePassed, declared, admissionEpoch))
	if err := require(); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("host B's pass = %v, want %v on the daemon's host", err, store.ErrBackendNotConformant)
	}
	recordConformanceOn(t, s, domain.DaemonHostID,
		hostRecord(domain.ConformancePassed, declared, admissionEpoch.Add(time.Minute)))
	recordConformanceOn(t, s, hostB, hostRecord(domain.ConformanceFailed, nil, admissionEpoch.Add(2*time.Minute)))
	if err := require(); err != nil {
		t.Fatalf("the daemon's pass after host B failed = %v, want admitted", err)
	}
}
