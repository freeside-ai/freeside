package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// openUnattendedNoConformance seeds everything an unattended admission needs
// except the backend-conformance record, so each test controls the
// conformance state it gates against.
func openUnattendedNoConformance(t *testing.T) (*store.Store, admissionFixture) {
	t.Helper()
	ctx := context.Background()
	f := unattendedAdmissionFixture(t)
	s := storetest.Open(t, tempDBPath(t), unattendedOptions())
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, f.identity, admissionEpoch)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
	return s, f
}

func recordConformance(t *testing.T, s *store.Store, record domain.BackendConformance) uint64 {
	t.Helper()
	var generation uint64
	if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		var err error
		generation, err = tx.RecordBackendConformance(context.Background(), record)
		return err
	}); err != nil {
		t.Fatalf("RecordBackendConformance: %v", err)
	}
	return generation
}

func conformanceAt(t *testing.T, outcome domain.ConformanceOutcome,
	caps domain.CapabilitySnapshot, at time.Time,
) domain.BackendConformance {
	t.Helper()
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: outcome,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities:        caps, ProvedAt: at,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	return record
}

// TestUnattendedAdmissionRequiresBackendConformance is #320's write-boundary
// gate end to end: no record fails closed, a failed record fails closed, a
// passed record admits, a failed append supersedes the pass and refuses even
// a byte-identical replay, and the lapse never makes the recorded admission
// unreadable.
func TestUnattendedAdmissionRequiresBackendConformance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, f := openUnattendedNoConformance(t)

	if err := recordAdmission(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("admission with no conformance record = %v, want %v", err, store.ErrBackendNotConformant)
	}

	failed := conformanceAt(t, domain.ConformanceFailed, nil, admissionEpoch)
	if got := recordConformance(t, s, failed); got != 1 {
		t.Fatalf("first generation = %d, want 1", got)
	}
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("admission under a failed record = %v, want %v", err, store.ErrBackendNotConformant)
	}

	passed := conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch.Add(time.Minute))
	if got := recordConformance(t, s, passed); got != 2 {
		t.Fatalf("second generation = %d, want 2", got)
	}
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("admission under a passed record: %v", err)
	}

	// A later failed pass supersedes the passed record under the same
	// latest-wins discipline as ward's in-memory generation guard, and a
	// byte-identical replay of the already-stored admission refuses rather
	// than converging (the RequireUnattendedAdmissible ordering rationale).
	superseding := conformanceAt(t, domain.ConformanceFailed, nil, admissionEpoch.Add(2*time.Minute))
	recordConformance(t, s, superseding)
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("replayed admission after a failed pass = %v, want %v", err, store.ErrBackendNotConformant)
	}

	// The lapse stops new admission, never reading history: the stored
	// admission still reconstructs (the frozen-admission decision, #301).
	var got domain.ExecutionAdmission
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetExecutionAdmission(ctx, f.admission.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("reconstruction after conformance lapse: %v", err)
	}
	if got.ID != f.admission.ID {
		t.Fatalf("reconstructed admission = %q, want %q", got.ID, f.admission.ID)
	}
}

// TestUnattendedAdmissionRefusedDuringPendingRecheck pins the begin-marker
// half: a recheck's superseding marker stops the previous passed row
// admitting for as long as the recheck is pending, closing the gap between a
// spawn-time snapshot freeze and the admission write.
func TestUnattendedAdmissionRefusedDuringPendingRecheck(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(time.Minute)))
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("admission during a pending recheck = %v, want %v", err, store.ErrBackendNotConformant)
	}
}

// TestUnattendedAdmissionExceedingConformanceRefused pins the over-claim
// half: a snapshot wider than the backend's proven declaration is refused at
// the write boundary even though it clears the configured floor.
func TestUnattendedAdmissionExceedingConformanceRefused(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)

	proven := domain.NewCapabilitySnapshot(domain.CapPostExitExport)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, proven, admissionEpoch))

	if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrAdmissionExceedsConformance) {
		t.Fatalf("admission beyond the proven declaration = %v, want %v",
			err, domain.ErrAdmissionExceedsConformance)
	}
}

// TestUnattendedAdmissionRequiresTheCurrentBackendConfiguration closes the
// cross-daemon race: a still-running daemon configured for A may not combine
// its in-memory capability declaration with the newest durable proof for B.
func TestUnattendedAdmissionRequiresTheCurrentBackendConfiguration(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(
		t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record admission under configuration A: %v", err)
	}

	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		Capabilities:        conformantCapabilities(t),
		ProvedAt:            admissionEpoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	recordConformance(t, s, record)

	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		admission, err := tx.GetExecutionAdmission(context.Background(), f.admission.InvocationID)
		if err != nil {
			return err
		}
		return tx.RequireBackendConformant(context.Background(), admission)
	}); !errors.Is(
		err, domain.ErrAdmissionConfigurationMismatch,
	) {
		t.Fatalf("replayed admission for configuration A under proof B = %v, want %v",
			err, domain.ErrAdmissionConfigurationMismatch)
	}
}

// authenticateConformance re-gates an already-recorded admission through the
// tolerant authenticate path (AuthenticateBackendConformant); requireConformance
// runs the strict mint path (RequireBackendConformant). The two share a core
// and differ only in how a same-configuration supersession marker is treated.
func authenticateConformance(t *testing.T, s *store.Store, admission domain.ExecutionAdmission) error {
	t.Helper()
	return s.Read(context.Background(), func(tx *store.ReadTx) error {
		return tx.AuthenticateBackendConformant(context.Background(), admission)
	})
}

func requireConformance(t *testing.T, s *store.Store, admission domain.ExecutionAdmission) error {
	t.Helper()
	return s.Read(context.Background(), func(tx *store.ReadTx) error {
		return tx.RequireBackendConformant(context.Background(), admission)
	})
}

// TestAuthenticateToleratesSameConfigurationRecheck is issue #761's core: an
// already-admitted invocation re-authenticates across its own daemon's startup
// re-proof of the same configuration (a supersession marker for the admission's
// digest) by re-binding to the passed proof the marker superseded. The mint
// gate stays strict against the identical state, so unadmitted work still holds
// out the window.
func TestAuthenticateToleratesSameConfigurationRecheck(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(time.Minute)))

	if err := authenticateConformance(t, s, f.admission); err != nil {
		t.Fatalf("authenticate during a same-configuration recheck = %v, want nil", err)
	}
	if err := requireConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("mint during a same-configuration recheck = %v, want %v", err, store.ErrBackendNotConformant)
	}
}

// TestAuthenticateSupersessionPreservesCeiling proves the recovered proof, not
// the capability-nil marker, supplies the ceiling: an admission wider than the
// superseded proof is refused as an over-claim, never silently authenticated by
// a dropped ceiling.
func TestAuthenticateSupersessionPreservesCeiling(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	proven := domain.NewCapabilitySnapshot(domain.CapPostExitExport)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, proven, admissionEpoch))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(time.Minute)))

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, domain.ErrAdmissionExceedsConformance) {
		t.Fatalf("authenticate beyond the superseded proof = %v, want %v",
			err, domain.ErrAdmissionExceedsConformance)
	}
}

// TestAuthenticateRefusesDifferentConfigurationSupersession keeps a
// reconfiguration fatal: a marker for a different configuration means the
// admission's configuration is no longer current, so tolerance never applies
// and the admission cannot start or recover.
func TestAuthenticateRefusesDifferentConfigurationSupersession(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	reconfigured, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformanceSuperseded,
		ConfigurationDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		ProvedAt:            admissionEpoch.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	recordConformance(t, s, reconfigured)

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate under a different-configuration recheck = %v, want %v",
			err, store.ErrBackendNotConformant)
	}
}

// TestAuthenticateRefusesFailedSameConfiguration keeps a failed proof fatal:
// tolerance is scoped to the supersession marker a recheck-in-progress writes,
// never a completed failure, which is a real non-conformance.
func TestAuthenticateRefusesFailedSameConfiguration(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceFailed, nil, admissionEpoch.Add(time.Minute)))

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate under a failed same-configuration proof = %v, want %v",
			err, store.ErrBackendNotConformant)
	}
}

// TestAuthenticateRefusesSupersessionWithoutPriorPass keeps tolerance closed
// when the marker superseded no passed proof this admission may re-bind to.
func TestAuthenticateRefusesSupersessionWithoutPriorPass(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch))

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate with no prior passed proof = %v, want %v",
			err, store.ErrBackendNotConformant)
	}
}

// TestAuthenticateRefusesInterveningFailedProof keeps a failed re-proof fatal
// even when an older pass survives in the log: passed, superseded, failed,
// superseded. Re-binding to the older pass over the intervening failure would
// authenticate an already-admitted invocation against a declaration that
// failure invalidated (a fail-open the mint gate refuses), so tolerance must
// re-bind to the declaration the marker superseded, which failed.
func TestAuthenticateRefusesInterveningFailedProof(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(time.Minute)))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceFailed, nil, admissionEpoch.Add(2*time.Minute)))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(3*time.Minute)))

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate over an intervening failed re-proof = %v, want %v",
			err, store.ErrBackendNotConformant)
	}
}

// TestAuthenticateRefusesInterveningOtherConfigurationPass keeps a rollback
// window fatal: passed(A), passed(B), superseded(A). The marker superseded the
// B declaration, not the older A pass, so an A-bound admission must not
// resurrect that A pass while no completed proof currently authorizes A. The
// recovery must find the actual superseded declaration (B) regardless of
// digest and refuse because it does not match the admission.
func TestAuthenticateRefusesInterveningOtherConfigurationPass(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	otherConfig, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		Capabilities:        conformantCapabilities(t),
		ProvedAt:            admissionEpoch.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	recordConformance(t, s, otherConfig)
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(2*time.Minute)))

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate over an intervening other-configuration pass = %v, want %v",
			err, store.ErrBackendNotConformant)
	}
}

// TestAuthenticateRefusesRestartStackedSupersession refuses when a restart
// mid-recheck stacks two markers: passed, superseded, superseded. The row the
// newest marker superseded is itself a marker, so the passing declaration was
// already cleared and no completed pass is current. Tolerance re-binds only to
// an immediately-preceding pass; here it refuses and the engine holds the run
// until the re-proof completes, which is conservative but never a fail-open.
func TestAuthenticateRefusesRestartStackedSupersession(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(time.Minute)))
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(2*time.Minute)))

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate across a restart-stacked recheck = %v, want %v",
			err, store.ErrBackendNotConformant)
	}
}

// TestAuthenticateRefusesStackedCrossConfigurationMarkers is the fail-open the
// digest-agnostic recovery closes: passed(A), superseded(B), superseded(A). The
// reconfiguration to B cleared A at the B marker and B never completed, so no
// pass currently authorizes A. Because the row the A marker superseded is the B
// marker, not the old A pass, tolerance must refuse rather than resurrect A.
func TestAuthenticateRefusesStackedCrossConfigurationMarkers(t *testing.T) {
	t.Parallel()
	s, f := openUnattendedNoConformance(t)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch))
	reconfigured, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformanceSuperseded,
		ConfigurationDigest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		ProvedAt:            admissionEpoch.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	recordConformance(t, s, reconfigured)
	recordConformance(t, s, conformanceAt(t, domain.ConformanceSuperseded, nil, admissionEpoch.Add(2*time.Minute)))

	if err := authenticateConformance(t, s, f.admission); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("authenticate across stacked cross-configuration markers = %v, want %v",
			err, store.ErrBackendNotConformant)
	}
}

// TestAttendedAdmissionNeedsNoConformance is the owner-ratified scope
// reading: §5.7 admits a weaker, unproven runner class for attended_dev, so
// the conformance gate applies to unattended admission only.
func TestAttendedAdmissionNeedsNoConformance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := storetest.Open(t, tempDBPath(t), store.Options{AdmissionFloors: attendedFloors()})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, f.identity, admissionEpoch)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("attended admission with no conformance record: %v", err)
	}
}

// TestRecordBackendConformanceRefusals is the accept-side boundary: a record
// the domain would not validate, an over-claim constructed directly rather
// than through the cooperative constructor, and a caller-supplied generation
// are all refused before the row exists.
func TestRecordBackendConformanceRefusals(t *testing.T) {
	t.Parallel()
	s, _ := openUnattendedNoConformance(t)
	record := func(c domain.BackendConformance) error {
		return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
			_, err := tx.RecordBackendConformance(context.Background(), c)
			return err
		})
	}

	overclaim := domain.BackendConformance{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities: domain.NewCapabilitySnapshot(
			domain.CapPostExitExport, domain.CapCredentialVolumeDetach),
		ProvedAt: admissionEpoch,
	}
	if err := record(overclaim); !errors.Is(err, domain.ErrConformanceOverclaim) {
		t.Errorf("over-claiming record = %v, want %v", err, domain.ErrConformanceOverclaim)
	}

	unknown := domain.BackendConformance{
		Backend: "novel_backend", Outcome: domain.ConformancePassed, ProvedAt: admissionEpoch,
	}
	if err := record(unknown); !errors.Is(err, domain.ErrInvalidRunnerBackendClass) {
		t.Errorf("unknown class = %v, want %v", err, domain.ErrInvalidRunnerBackendClass)
	}

	failedWithCaps := domain.BackendConformance{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformanceFailed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities:        domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		ProvedAt:            admissionEpoch,
	}
	if err := record(failedWithCaps); !errors.Is(err, domain.ErrConformanceCapabilitiesWithoutPass) {
		t.Errorf("failed record with capabilities = %v, want %v",
			err, domain.ErrConformanceCapabilitiesWithoutPass)
	}

	forgedGeneration := conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch)
	forgedGeneration.Generation = 41
	if err := record(forgedGeneration); !errors.Is(err, store.ErrConformanceGenerationSupplied) {
		t.Errorf("caller-supplied generation = %v, want %v", err, store.ErrConformanceGenerationSupplied)
	}

	unbound := conformanceAt(t, domain.ConformancePassed, conformantCapabilities(t), admissionEpoch)
	unbound.ConfigurationDigest = domain.UnboundBackendConfigurationDigest
	if err := record(unbound); !errors.Is(err, domain.ErrConformanceConfigurationUnbound) {
		t.Errorf("unbound configuration = %v, want %v",
			err, domain.ErrConformanceConfigurationUnbound)
	}
}

// TestLatestBackendConformanceAbsence pins the Lookup shape: absence is a
// boolean, per backend, never an error.
func TestLatestBackendConformanceAbsence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openUnattendedNoConformance(t)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, found, err := tx.LatestBackendConformance(ctx, domain.BackendFreshVMReadOnlyVolumeHandoff)
		if err != nil {
			return err
		}
		if found {
			t.Error("empty log reported a conformance record")
		}
		return nil
	}); err != nil {
		t.Fatalf("LatestBackendConformance: %v", err)
	}
}
