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
	s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: tamperFloor(),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
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

// legacyIdentityAdmission is the pre-cutover shape for the same attempt: an
// identity and no agent binding.
func legacyIdentityAdmission(t *testing.T) domain.ExecutionAdmission {
	t.Helper()
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
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	return legacy
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
	legacy := legacyIdentityAdmission(t)
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

// TestMarkedGenerationRefusesNewAdmission is the admitting transaction's half
// of rule 4. The admission is built directly for the marked generation, the
// way a caller that skipped role resolution would: it carries no marked or
// unmarked claim, and the store reads the mark rows itself.
func TestMarkedGenerationRefusesNewAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, finding := range domain.AllCredentialIntegrityFindings {
		t.Run(string(finding), func(t *testing.T) {
			s := openAgentAdmissionStore(t)
			generation := seedAgentClosure(t, s)
			mark := recordIntegrityMark(t, s, integrityMarkFor(generation, finding))
			admission := agentBoundAdmission(t, generation)
			err := s.Write(ctx, func(tx *WriteTx) error {
				return tx.RecordExecutionAdmission(ctx, admission)
			})
			assertMarkedRefusal(t, err, mark)
			if err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetExecutionAdmission(ctx, admission.InvocationID)
				return err
			}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused admission read = %v, want %v", err, ErrNotFound)
			}
		})
	}

	// The admitting transaction reads the same rows the gate does, so an
	// unreadable mark fails it closed instead of admitting past the row.
	t.Run("an unreadable mark row", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		recordIntegrityMark(t, s, integrityMarkFor(generation, domain.CredentialIntegrityTruncation))
		if _, err := s.db.ExecContext(ctx,
			`UPDATE generation_integrity_marks SET observed_at = '2026-01-02T04:00:00Z'`); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		admission := agentBoundAdmission(t, generation)
		err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, admission)
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Fatalf("record past a tampered mark row = %v, want %v", err, errRowInconsistent)
		}
	})

	// Re-enrollment clears the refusal: generation 2 has no mark, while an
	// admission naming generation 1 is still refused.
	t.Run("the re-enrolled generation admits", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		first := seedAgentClosure(t, s)
		mark := recordIntegrityMark(t, s, integrityMarkFor(first, domain.CredentialIntegrityTruncation))
		second := appendSecondGeneration(t, s)
		err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, agentBoundAdmission(t, first))
		})
		assertMarkedRefusal(t, err, mark)
		if err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, agentBoundAdmission(t, second))
		}); err != nil {
			t.Fatalf("record admission on the re-enrolled generation: %v", err)
		}
	})

	t.Run("a legacy admission names no generation", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		recordIntegrityMark(t, s, integrityMarkFor(generation, domain.CredentialIntegrityTruncation))
		legacy := legacyIdentityAdmission(t)
		if err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, legacy)
		}); err != nil {
			t.Fatalf("record legacy admission beside a marked generation: %v", err)
		}
	})
}

// TestLaterMarkKeepsRecordedAdmissionReadable pins the boundary the mark
// stops at: it refuses what starts next, and never makes an admission
// recorded before it unreadable (the RequireBackendConformant rule).
func TestLaterMarkKeepsRecordedAdmissionReadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAgentAdmissionStore(t)
	generation := seedAgentClosure(t, s)
	admission := agentBoundAdmission(t, generation)
	record := func() error {
		return s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordExecutionAdmission(ctx, admission)
		})
	}
	if err := record(); err != nil {
		t.Fatalf("record admission: %v", err)
	}
	recordIntegrityMark(t, s, integrityMarkFor(generation, domain.CredentialIntegrityCorruption))

	var (
		got    domain.ExecutionAdmission
		listed []domain.ExecutionAdmission
	)
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		if got, err = tx.GetExecutionAdmission(ctx, admission.InvocationID); err != nil {
			return err
		}
		listed, err = tx.ListRunExecutionAdmissions(ctx, admission.RunID)
		return err
	}); err != nil {
		t.Fatalf("read an admission recorded before the mark: %v", err)
	}
	if got.ID != admission.ID || len(listed) != 1 || listed[0].ID != admission.ID {
		t.Fatalf("read admission %q, listed %d", got.ID, len(listed))
	}
	if err := record(); err != nil {
		t.Fatalf("byte-identical replay after the mark: %v", err)
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

// agentAdmissionCiting rebuilds the agent-bound admission with a binding that
// names a task line as the selection that chose its agent.
func agentAdmissionCiting(
	t *testing.T, generation domain.EnrollmentGeneration, lineID domain.Digest,
) domain.ExecutionAdmission {
	t.Helper()
	admission := agentBoundAdmission(t, generation)
	binding := *admission.AgentBinding
	binding.SelectionSource, binding.SelectionRecordID = domain.AgentSelectionSourceTaskLine, lineID
	cited, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: admission.InvocationID, RunID: admission.RunID,
		StageID: admission.StageID, AttemptID: admission.AttemptID,
		Backend: admission.Backend, Capabilities: admission.Capabilities,
		OperatingMode: admission.OperatingMode, CredentialMode: admission.CredentialMode,
		EgressProfile: admission.EgressProfile, ImageRef: admission.ImageRef,
		SpecDigest: admission.SpecDigest, PolicyDigest: admission.PolicyDigest,
		InputDigest: admission.InputDigest, StageInputs: admission.StageInputs,
		Base: admission.Base, Workspace: admission.Workspace,
		AuthIdentityID: admission.AuthIdentityID, AgentBinding: &binding,
		AdmittedAt: admission.AdmittedAt,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	return cited
}

// appendRunTaskLine records an implementer line on the run's task.
func appendRunTaskLine(t *testing.T, s *Store, runID domain.RunID, agent string) domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	var line domain.TaskLine
	if err := s.Write(ctx, func(tx *WriteTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		line, err = tx.AppendTaskLine(ctx, domain.TaskLineInput{
			TaskID: run.TaskID, Role: domain.RoleImplementer, Agent: agent,
			Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
		}, time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatalf("append task line: %v", err)
	}
	return line
}

// TestTaskLineAdmissionResolvesTheLineItCites pins the re-gate of a binding
// under task_line (plan §5.4): the binding's own validation accepts any
// well-formed digest, so the store resolves the id to a recorded line of the
// admission run's task, when the admission is recorded and on every read.
func TestTaskLineAdmissionResolvesTheLineItCites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	record := func(s *Store, admission domain.ExecutionAdmission) error {
		return s.Write(ctx, func(tx *WriteTx) error { return tx.RecordExecutionAdmission(ctx, admission) })
	}
	read := func(s *Store) (domain.ExecutionAdmission, error) {
		var got domain.ExecutionAdmission
		err := s.Read(ctx, func(tx *ReadTx) error {
			var err error
			got, err = tx.GetExecutionAdmission(ctx, "inv-1")
			return err
		})
		return got, err
	}

	t.Run("a recorded line of the run's task", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		cited := appendRunTaskLine(t, s, "run-1", "codex")
		// A later version does not unseat the line the attempt was admitted
		// under.
		appendRunTaskLine(t, s, "run-1", "claude-b")
		admission := agentAdmissionCiting(t, generation, cited.ID)
		if err := record(s, admission); err != nil {
			t.Fatalf("record = %v", err)
		}
		got, err := read(s)
		if err != nil || got.ID != admission.ID || !reflect.DeepEqual(got.AgentBinding, admission.AgentBinding) {
			t.Fatalf("read back = %+v, %v", got.AgentBinding, err)
		}
		// The line removed afterwards: the stored admission no longer
		// reconstructs.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM task_lines`); err != nil {
			t.Fatal(err)
		}
		if _, err := read(s); !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("read after the line was removed = %v, want %v", err, domain.ErrAdmissionDerivationMismatch)
		}
	})

	t.Run("an id no line was recorded under", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		appendRunTaskLine(t, s, "run-1", "codex")
		err := record(s, agentAdmissionCiting(t, generation,
			"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"))
		// Not ErrNotFound: a caller asking whether an admission exists must
		// not read a refused one as absent.
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) || errors.Is(err, ErrNotFound) {
			t.Fatalf("record = %v, want %v and not %v", err, domain.ErrAdmissionDerivationMismatch, ErrNotFound)
		}
	})

	t.Run("another task's line", func(t *testing.T) {
		s := openAgentAdmissionStore(t)
		generation := seedAgentClosure(t, s)
		if err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.PutRun(ctx, domain.Run{
				ID: "run-2", ProjectID: "proj-1", SpecDigest: agentSpecDigest, PolicyDigest: agentPolicyDigest,
			})
		}); err != nil {
			t.Fatal(err)
		}
		other := appendRunTaskLine(t, s, "run-2", "codex")
		err := record(s, agentAdmissionCiting(t, generation, other.ID))
		if !errors.Is(err, domain.ErrAdmissionDerivationMismatch) {
			t.Fatalf("record = %v, want %v", err, domain.ErrAdmissionDerivationMismatch)
		}
	})
}
