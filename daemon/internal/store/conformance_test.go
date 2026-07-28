package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// openUnattendedNoConformance seeds everything an unattended admission needs
// except the backend-conformance record, so each test controls the
// conformance state it gates against.
func openUnattendedNoConformance(t *testing.T) (*store.Store, admissionFixture) {
	t.Helper()
	ctx := context.Background()
	f := unattendedAdmissionFixture(t)
	s, err := store.Open(ctx, tempDBPath(t), unattendedOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
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
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome: outcome, Capabilities: caps, ProvedAt: at,
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
	s, f := openUnattendedNoConformance(t)

	proven := domain.NewCapabilitySnapshot(domain.CapPostExitExport)
	recordConformance(t, s, conformanceAt(t, domain.ConformancePassed, proven, admissionEpoch))

	if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrAdmissionExceedsConformance) {
		t.Fatalf("admission beyond the proven declaration = %v, want %v",
			err, domain.ErrAdmissionExceedsConformance)
	}
}

// TestAttendedAdmissionNeedsNoConformance is the owner-ratified scope
// reading: §5.7 admits a weaker, unproven runner class for attended_dev, so
// the conformance gate applies to unattended admission only.
func TestAttendedAdmissionNeedsNoConformance(t *testing.T) {
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s, err := store.Open(ctx, tempDBPath(t), store.Options{AdmissionFloors: attendedFloors()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
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
	s, _ := openUnattendedNoConformance(t)
	record := func(c domain.BackendConformance) error {
		return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
			_, err := tx.RecordBackendConformance(context.Background(), c)
			return err
		})
	}

	overclaim := domain.BackendConformance{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome: domain.ConformancePassed,
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
		Backend:      domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:      domain.ConformanceFailed,
		Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		ProvedAt:     admissionEpoch,
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
}

// TestLatestBackendConformanceAbsence pins the Lookup shape: absence is a
// boolean, per backend, never an error.
func TestLatestBackendConformanceAbsence(t *testing.T) {
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
