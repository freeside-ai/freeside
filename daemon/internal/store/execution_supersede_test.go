package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// supersedeConfigDigest is the running configuration a supersession admits
// under; the fixture's stored admission binds seedBackendConformance's.
const supersedeConfigDigest = domain.Digest("sha256:2222222222222222222222222222222222222222222222222222222222222222")

// supersedeFixture is a store holding one recorded, never-started unattended
// admission, and the admission of the same attempt under another
// configuration.
type supersedeFixture struct {
	store *store.Store
	stale domain.ExecutionAdmission
	fresh domain.ExecutionAdmission
}

func unattendedSupersedeOptions() store.Options {
	return store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
			domain.ModeUnattended:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource:      healthyBackupHealthSource(),
	}
}

// seedConformanceFor records a passed conformance record for digest, so an
// admission bound to it passes the backend gate.
func seedConformanceFor(t *testing.T, s *store.Store, digest domain.Digest) {
	t.Helper()
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: digest, Capabilities: conformantCapabilities(t), ProvedAt: admissionEpoch,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(context.Background(), domain.DaemonHostID, record)
		return err
	}); err != nil {
		t.Fatalf("seed conformance for %s: %v", digest, err)
	}
}

// newSupersedeFixture records the stale admission; staleMutate and
// freshMutate adjust the two admissions' inputs, and freshConformant says
// whether the fresh configuration has a passed conformance record.
func newSupersedeFixture(
	t *testing.T, staleMutate, freshMutate func(*domain.ExecutionAdmissionInput), freshConformant bool,
) supersedeFixture {
	t.Helper()
	profile := testTrustProfile(t, "owner/repo", 424242)
	unattended := func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &profile.ProfileDigest
	}
	f := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		unattended(in)
		if staleMutate != nil {
			staleMutate(in)
		}
	})
	fresh := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		unattended(in)
		in.BackendConfigurationDigest = supersedeConfigDigest
		in.AdmittedAt = admissionEpoch.Add(time.Hour)
		if freshMutate != nil {
			freshMutate(in)
		}
	}).admission
	s := openWithFixture(t, f, unattendedSupersedeOptions())
	recordTrustProfileRevision(t, s, profile, admissionEpoch)
	if freshConformant {
		seedConformanceFor(t, s, supersedeConfigDigest)
	}
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record stale admission: %v", err)
	}
	return supersedeFixture{store: s, stale: f.admission, fresh: fresh}
}

func (f supersedeFixture) supersede(t *testing.T) error {
	t.Helper()
	return f.store.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.SupersedeUnstartedExecutionAdmission(context.Background(), f.stale, f.fresh, nil)
	})
}

// requireAdmissions fails t unless the invocation's stored admission is
// current and its superseded history is exactly superseded.
func (f supersedeFixture) requireAdmissions(
	t *testing.T, current domain.ExecutionAdmission, superseded ...domain.ExecutionAdmission,
) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetExecutionAdmissionRecord(ctx, f.stale.InvocationID)
		if err != nil {
			return err
		}
		if got.ID != current.ID {
			t.Errorf("stored admission = %s, want %s", got.ID, current.ID)
		}
		history, err := tx.ListSupersededExecutionAdmissions(ctx, f.stale.InvocationID)
		if err != nil {
			return err
		}
		if len(history) != len(superseded) {
			t.Fatalf("superseded admissions = %d, want %d", len(history), len(superseded))
		}
		for i := range history {
			if history[i].ID != superseded[i].ID {
				t.Errorf("superseded[%d] = %s, want %s", i, history[i].ID, superseded[i].ID)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSupersedeUnstartedExecutionAdmission: a never-started unattended
// attempt's admission is replaced by its admission under the running
// configuration, the replaced record stays in the history, and the fresh
// record is the one every later read and gate sees.
func TestSupersedeUnstartedExecutionAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSupersedeFixture(t, nil, nil, true)
	if err := f.supersede(t); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	f.requireAdmissions(t, f.fresh, f.stale)
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetExecutionAdmission(ctx, f.stale.InvocationID)
		if err != nil {
			return err
		}
		if got.BackendConfigurationDigest != supersedeConfigDigest {
			t.Errorf("re-gated admission binds %s, want %s", got.BackendConfigurationDigest, supersedeConfigDigest)
		}
		return nil
	}); err != nil {
		t.Fatalf("re-gated read of the fresh admission: %v", err)
	}
	// The fresh record is write-once like any other: an exact replay
	// converges and the superseded record no longer does.
	if err := recordAdmission(t, f.store, f.fresh); err != nil {
		t.Fatalf("replay of the fresh admission: %v", err)
	}
	if err := recordAdmission(t, f.store, f.stale); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("replay of the superseded admission = %v, want %v", err, store.ErrImmutableConflict)
	}
}

// TestSupersedeRefusesAFreshAdmissionNewAdmissionGatesRefuse: the fresh
// admission passes the gates a new one does, and a refusal changes nothing.
func TestSupersedeRefusesAFreshAdmissionNewAdmissionGatesRefuse(t *testing.T) {
	t.Parallel()
	f := newSupersedeFixture(t, nil, nil, false)
	if err := f.supersede(t); !errors.Is(err, store.ErrBackendNotConformant) {
		t.Fatalf("supersede under an unproved configuration = %v, want %v", err, store.ErrBackendNotConformant)
	}
	f.requireAdmissions(t, f.stale)
}

// TestSupersedeRefusesAnythingButAnUnstartedReconfiguredAttempt: each case
// breaks one condition of the one write-once exception, and each refusal
// leaves the stored admission and its history as they were.
func TestSupersedeRefusesAnythingButAnUnstartedReconfiguredAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		stale func(*domain.ExecutionAdmissionInput)
		fresh func(*domain.ExecutionAdmissionInput)
		// started records what the attempt left behind, against the
		// stored admission.
		started func(*testing.T, *store.Store, domain.ExecutionAdmission)
		// mismatch passes a stale admission other than the stored one.
		mismatch bool
	}{
		{name: "attended stored admission", stale: func(in *domain.ExecutionAdmissionInput) {
			in.OperatingMode = domain.ModeAttendedDev
			in.TrustProfileDigest = nil
			in.Capabilities = domain.NewCapabilitySnapshot(domain.CapDetachableWorkspace, domain.CapPostExitExport)
		}},
		{name: "same configuration", fresh: func(in *domain.ExecutionAdmissionInput) {
			in.BackendConfigurationDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		}},
		{name: "another invocation", fresh: func(in *domain.ExecutionAdmissionInput) { in.InvocationID = "inv-2" }},
		{name: "another run", fresh: func(in *domain.ExecutionAdmissionInput) { in.RunID = "run-2" }},
		{name: "another stage", fresh: func(in *domain.ExecutionAdmissionInput) { in.StageID = "stage-2" }},
		{name: "another attempt", fresh: func(in *domain.ExecutionAdmissionInput) { in.AttemptID = "attempt-2" }},
		{name: "another base", fresh: func(in *domain.ExecutionAdmissionInput) { in.Base.BaseSHA = "feedface" }},
		{name: "not the stored admission", mismatch: true},
		{name: "export recorded", started: func(t *testing.T, s *store.Store, a domain.ExecutionAdmission) {
			recordAdmissionExport(t, s, a)
		}},
		{name: "outcome recorded", started: func(t *testing.T, s *store.Store, a domain.ExecutionAdmission) {
			t.Helper()
			if err := s.Write(ctx, func(tx *store.WriteTx) error {
				return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
					InvocationID: a.InvocationID, AdmissionID: a.ID,
					Status: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseUnknown,
					Summary: "writer failed", RecordedAt: admissionEpoch.Add(time.Minute),
				})
			}); err != nil {
				t.Fatalf("record outcome: %v", err)
			}
		}},
		{name: "import started", started: func(t *testing.T, s *store.Store, a domain.ExecutionAdmission) {
			t.Helper()
			if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				return tx.RecordCurrentImportStart(ctx, domain.CurrentImportStart{
					InvocationID: a.InvocationID, AdmissionID: a.ID,
				})
			}); err != nil {
				t.Fatalf("record import start: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSupersedeFixture(t, tc.stale, tc.fresh, true)
			stored := f.stale
			if tc.started != nil {
				tc.started(t, f.store, f.stale)
			}
			if tc.mismatch {
				f.stale = newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
					in.OperatingMode = domain.ModeUnattended
					in.Capabilities = conformantCapabilities(t)
					in.TrustProfileDigest = stored.TrustProfileDigest
					in.Workspace = "ws-other"
				}).admission
			}
			if err := f.supersede(t); !errors.Is(err, store.ErrAdmissionNotSupersedable) {
				t.Fatalf("supersede = %v, want %v", err, store.ErrAdmissionNotSupersedable)
			}
			f.stale = stored
			f.requireAdmissions(t, stored)
		})
	}
}
