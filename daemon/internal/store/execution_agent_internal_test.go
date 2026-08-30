package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Valid content addresses shared by the run and admission fixtures: the v4
// admission carries a stage-input snapshot, whose digests must be canonical
// and must equal the admission's spec/policy/input bindings.
const (
	agentSpecDigest   = "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	agentPolicyDigest = "sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"
	agentInputDigest  = "sha256:" + "4444444444444444444444444444444444444444444444444444444444444444"
)

func agentStageInputs(t *testing.T) domain.StageInputSnapshot {
	t.Helper()
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         agentInputDigest,
		SpecificationDigest: agentSpecDigest,
		PromptPackageDigest: "sha256:5555555555555555555555555555555555555555555555555555555555555555",
		PolicyDigest:        agentPolicyDigest,
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorCodex,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// The v4 agent-bound admission and the adapter-conformance log (plan §5.4,
// issue #894), at the store boundary: the binding's referents are re-gated on
// every write and read, the extracted columns are cross-checked, and the
// legacy reader keeps pre-cutover admissions exactly as recorded.

func openAgentAdmissionStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: tamperFloor(),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedAgentClosure records the identity, enrollment, lease, and one store
// generation an agent-bound admission binds, plus the run its attempt lives
// on, and returns the appended generation.
func seedAgentClosure(t *testing.T, s *Store) domain.EnrollmentGeneration {
	t.Helper()
	ctx := context.Background()
	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: agentSpecDigest, PolicyDigest: agentPolicyDigest,
		Stages: []domain.Stage{{
			ID: "stage-1", RunID: "run-1", Name: "implementation",
			Attempts: []domain.Attempt{{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}},
		}},
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}
	recordEnrollmentFixtures(t, s)
	leaseStart := time.Date(2026, 1, 2, 3, 2, 0, 0, time.UTC)
	var stamped domain.EnrollmentGeneration
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		binding := &domain.LeaseGenerationBinding{
			EnrollmentID: "enroll-1", Generation: 0,
			AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
		}
		if _, err := tx.AcquireAuthStoreMutationLeaseBound(
			ctx, "auth-1", "inv-refresh", binding, leaseStart, leaseStart.Add(10*time.Minute)); err != nil {
			return err
		}
		var err error
		stamped, err = tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), leaseStart.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("seed enrollment generation: %v", err)
	}
	return stamped
}

func agentBoundAdmission(t *testing.T, generation domain.EnrollmentGeneration) domain.ExecutionAdmission {
	t.Helper()
	identityID := domain.AuthIdentityID("auth-1")
	stageInputs := agentStageInputs(t)
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     agentSpecDigest, PolicyDigest: agentPolicyDigest, InputDigest: agentInputDigest,
		StageInputs: &stageInputs,
		Base:        domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:   "ws-1", AuthIdentityID: &identityID,
		AgentBinding: &domain.AdmissionAgentBinding{
			AgentDigest:          "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			LaunchDigest:         "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			TreatmentDigest:      "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			PricingRevision:      "pricing-2026-01",
			LineupRevision:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			EnrollmentID:         generation.EnrollmentID,
			EnrollmentGeneration: generation.Ordinal,
			StoreManifestDigest:  generation.StoreManifestDigest,
			EffectiveEgress:      []string{"chatgpt.com"},
			Attended:             true,
		},
		AdmittedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	return admission
}

// TestAgentBoundAdmissionRoundTrips is the acceptance fixture: the new
// encoding round-trips through the store with its binding re-gated against
// the enrollment records it names.
func TestAgentBoundAdmissionRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAgentAdmissionStore(t)
	generation := seedAgentClosure(t, s)
	admission := agentBoundAdmission(t, generation)
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("record agent-bound admission: %v", err)
	}
	var got domain.ExecutionAdmission
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetExecutionAdmission(ctx, admission.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("read agent-bound admission: %v", err)
	}
	if got.ID != admission.ID || got.AgentBinding == nil ||
		!reflect.DeepEqual(got.AgentBinding, admission.AgentBinding) {
		t.Fatalf("round-trip = %+v", got.AgentBinding)
	}
}

// TestAgentBoundAdmissionGateFailsClosed pins the write-time re-gate: a
// binding whose referents are absent or disagree is refused, never recorded.
func TestAgentBoundAdmissionGateFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("manifest disagrees with the generation", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		generation.StoreManifestDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		admission := agentBoundAdmission(t, generation)
		err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, admission)
		})
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("record = %v, want %v", err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("generation the enrollment never appended", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		generation.Ordinal = 7
		admission := agentBoundAdmission(t, generation)
		err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, admission)
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("record = %v, want %v", err, ErrNotFound)
		}
	})

	t.Run("credential mode disagrees with the enrollment", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		admission := agentBoundAdmission(t, generation)
		// Rebuild the admission with a divergent credential mode; the
		// enrollment carries subscription_contained.
		identityID := domain.AuthIdentityID("auth-1")
		divergent, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
			InvocationID: admission.InvocationID, RunID: admission.RunID,
			StageID: admission.StageID, AttemptID: admission.AttemptID,
			Backend:        admission.Backend,
			Capabilities:   admission.Capabilities,
			OperatingMode:  admission.OperatingMode,
			CredentialMode: domain.CredentialLocalTrusted,
			EgressProfile:  admission.EgressProfile,
			ImageRef:       admission.ImageRef,
			SpecDigest:     admission.SpecDigest, PolicyDigest: admission.PolicyDigest,
			InputDigest: admission.InputDigest, StageInputs: admission.StageInputs,
			Base: admission.Base, Workspace: admission.Workspace,
			AuthIdentityID: &identityID,
			AgentBinding:   admission.AgentBinding,
			AdmittedAt:     admission.AdmittedAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, divergent)
		})
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("record = %v, want %v", err, domain.ErrAdmissionDerivationMismatch)
		}
	})
}

// TestAgentBindingColumnsCrossChecked pins that the extracted agent columns
// are authenticated like every other key column.
func TestAgentBindingColumnsCrossChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAgentAdmissionStore(t)
	generation := seedAgentClosure(t, s)
	admission := agentBoundAdmission(t, generation)
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE execution_admissions SET agent_digest = NULL WHERE invocation_id = 'inv-1'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, "inv-1")
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("tampered column read = %v, want %v", err, errRowInconsistent)
	}
}

// TestLegacyAdmissionKeepsItsRecord pins the permanent legacy rule at the
// store: an admission carrying an identity but no agent binding reconstructs
// exactly as recorded — NULL agent columns, no enrollment resolution — even
// while enrollments exist in the same store.
func TestLegacyAdmissionKeepsItsRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAgentAdmissionStore(t)
	generation := seedAgentClosure(t, s)
	_ = generation
	identityID := domain.AuthIdentityID("auth-1")
	legacy, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     agentSpecDigest, PolicyDigest: agentPolicyDigest, InputDigest: agentInputDigest,
		Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace: "ws-1", AuthIdentityID: &identityID,
		AdmittedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, legacy)
	}); err != nil {
		t.Fatalf("record legacy admission: %v", err)
	}
	var got domain.ExecutionAdmission
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetExecutionAdmission(ctx, legacy.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("read legacy admission: %v", err)
	}
	if got.ID != legacy.ID || got.AgentBinding != nil {
		t.Fatalf("legacy admission = %+v", got.AgentBinding)
	}
	var agentDigest, enrollmentID any
	if err := s.db.QueryRowContext(ctx,
		`SELECT agent_digest, enrollment_id FROM execution_admissions WHERE invocation_id = 'inv-1'`).
		Scan(&agentDigest, &enrollmentID); err != nil {
		t.Fatal(err)
	}
	if agentDigest != nil || enrollmentID != nil {
		t.Fatalf("legacy admission carries agent columns: %v, %v", agentDigest, enrollmentID)
	}
}

// TestAdapterConformanceStore pins the append-only adapter log: the
// store-assigned generation, the newest-row read, and fail-closed
// reconstruction of a tampered row.
func TestAdapterConformanceStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openEnrollmentStore(t)
	adapterDigest := domain.Digest("sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	record, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
		AdapterDigest: adapterDigest,
		Outcome:       domain.ConformancePassed,
		ProvedCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapInstructionDelivery,
			domain.LaunchCapRouteStoreContract,
		),
		ProvedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	var generation uint64
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		var err error
		generation, err = tx.RecordAdapterConformance(ctx, record)
		return err
	}); err != nil {
		t.Fatalf("record adapter conformance: %v", err)
	}
	if generation != 1 {
		t.Fatalf("assigned generation = %d, want 1", generation)
	}

	prestamped := record
	prestamped.Generation = 7
	err = s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordAdapterConformance(ctx, prestamped)
		return err
	})
	if !errors.Is(err, ErrAdapterGenerationSupplied) {
		t.Fatalf("prestamped record = %v, want %v", err, ErrAdapterGenerationSupplied)
	}

	superseding, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
		AdapterDigest: adapterDigest,
		Outcome:       domain.ConformanceSuperseded,
		ProvedAt:      time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.RecordAdapterConformance(ctx, superseding)
		return err
	}); err != nil {
		t.Fatalf("record superseding marker: %v", err)
	}
	var latest domain.AdapterConformance
	var found bool
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		latest, found, err = tx.LatestAdapterConformance(ctx, adapterDigest)
		return err
	}); err != nil {
		t.Fatalf("latest adapter conformance: %v", err)
	}
	if !found || latest.Outcome != domain.ConformanceSuperseded || latest.Generation != 2 {
		t.Fatalf("latest = %+v, found %v", latest, found)
	}

	if err := s.Read(ctx, func(tx *ReadTx) error {
		_, found, err := tx.LatestAdapterConformance(ctx, "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
		if err != nil {
			return err
		}
		if found {
			t.Fatal("absent adapter reported a record")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE adapter_conformance_records SET proved_capabilities = '["telepathy"]' WHERE id = 1`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, _, err := tx.LatestAdapterConformance(ctx, adapterDigest)
		return err
	})
	// The tampered row is generation 2's predecessor; the latest read still
	// reconstructs generation 2, so tamper the newest row instead.
	if err != nil {
		t.Fatalf("latest after tampering an older row = %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE adapter_conformance_records SET proved_capabilities = '["telepathy"]', outcome = 'passed' WHERE id = 2`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, _, err := tx.LatestAdapterConformance(ctx, adapterDigest)
		return err
	})
	if !errors.Is(err, domain.ErrInvalidLaunchCapability) {
		t.Fatalf("tampered newest row = %v, want %v", err, domain.ErrInvalidLaunchCapability)
	}
}
