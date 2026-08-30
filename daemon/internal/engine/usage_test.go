package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	usageTestSpecDigest   = domain.Digest("sha256:" + "2" + "222222222222222222222222222222222222222222222222222222222222222")
	usageTestPolicyDigest = domain.Digest("sha256:" + "3" + "333333333333333333333333333333333333333333333333333333333333333")
	usageTestInputDigest  = domain.Digest("sha256:" + "4" + "444444444444444444444444444444444444444444444444444444444444444")
	usageTestManifest     = domain.Digest("sha256:" + "5" + "555555555555555555555555555555555555555555555555555555555555555")
)

func usageEngineFixture(t *testing.T, bound bool) (*Engine, domain.Run, domain.Attempt) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	attempt := domain.Attempt{
		ID: "attempt-usage-1", StageID: "stage-usage-1", Number: 1, InvocationID: "inv-usage-1",
	}
	run := domain.Run{
		ID: "run-usage-1", ProjectID: "project-usage-1",
		SpecDigest: usageTestSpecDigest, PolicyDigest: usageTestPolicyDigest,
		Stages: []domain.Stage{{
			ID: attempt.StageID, RunID: "run-usage-1", Name: "implementation",
			Attempts: []domain.Attempt{attempt},
		}},
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}

	identity := domain.AuthIdentity{
		ID: "auth-usage-1", Provider: "openai", AccountBinding: "acct-usage-1",
		UsagePool: "pool-usage-1", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true,
	}
	enrollment := domain.ClientEnrollment{
		ID: "enrollment-usage-1", AuthIdentityID: identity.ID,
		HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
		AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: identity.AccountBinding,
	}
	recordedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var generation domain.EnrollmentGeneration
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, identity, recordedAt.Add(-4*time.Minute)); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, enrollment, recordedAt.Add(-3*time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, "inv-store-usage-1",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, Generation: 0,
				AuthStoreVolume: "codex-usage-store", StoreManifestDigest: usageTestManifest,
			}, recordedAt.Add(-2*time.Minute), recordedAt.Add(10*time.Minute))
		if err != nil {
			return err
		}
		expiry := recordedAt.Add(24 * time.Hour)
		generation, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: "codex-usage-store",
			StoreManifestDigest: usageTestManifest, LeaseFence: lease.Fence,
			AccountBinding: identity.AccountBinding, TokenExpiry: &expiry, RecordedAt: recordedAt,
		}, recordedAt.Add(-time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	stageInputs, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest: usageTestInputDigest, SpecificationDigest: usageTestSpecDigest,
		PromptPackageDigest: domain.Digest("sha256:" + strings.Repeat("6", 64)),
		PolicyDigest:        usageTestPolicyDigest,
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor: domain.AgentVendorCodex, Delivery: domain.VendorInstructionDeliveryAppendFile,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := &domain.AdmissionAgentBinding{
		AgentDigest:     domain.Digest("sha256:" + strings.Repeat("a", 64)),
		LaunchDigest:    domain.Digest("sha256:" + strings.Repeat("b", 64)),
		TreatmentDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)), PricingRevision: "pricing-2026-01",
		LineupRevision: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		EnrollmentID:   enrollment.ID, EnrollmentGeneration: generation.Ordinal,
		StoreManifestDigest: generation.StoreManifestDigest,
		EffectiveEgress:     []string{"chatgpt.com"}, Attended: true,
	}
	if !bound {
		binding = nil
	}
	identityID := identity.ID
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: attempt.InvocationID, RunID: run.ID, StageID: attempt.StageID, AttemptID: attempt.ID,
		Backend:       "fresh_vm_read_only_volume_handoff",
		Capabilities:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:    usageTestSpecDigest, PolicyDigest: usageTestPolicyDigest, InputDigest: usageTestInputDigest,
		StageInputs: &stageInputs,
		Base:        domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:   "workspace-usage-1", AuthIdentityID: &identityID,
		AgentBinding: binding, AdmittedAt: recordedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatal(err)
	}

	driver := fake.NewStageDriver()
	driver.Script(attempt.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Usage: []exec.UsageMeasurement{{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "input_tokens", Unit: "tokens", Quantity: 17, Sequence: 1, ObservedAt: recordedAt,
		}}},
	})
	if err := driver.Start(ctx, attempt.InvocationID, exec.StartSpec{}); err != nil {
		t.Fatal(err)
	}
	return &Engine{store: st, driver: driver}, run, attempt
}

func TestUsageObservationsPersistAfterStageCollect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bound bool
		want  int
	}{
		{"bound admission", true, 1},
		{"unbound admission", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, run, attempt := usageEngineFixture(t, tc.bound)
			var afterFirst store.ServerState
			for collect := 0; collect < 2; collect++ {
				if _, ready, err := engine.collectTerminal(context.Background(), run.ID, attempt); err != nil || !ready {
					t.Fatalf("collect %d = ready %v, err %v", collect+1, ready, err)
				}
				state, err := engine.store.ServerState(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if collect == 0 {
					afterFirst = state
				} else if state != afterFirst {
					t.Fatalf("duplicate collect changed server state from %+v to %+v", afterFirst, state)
				}
			}
			var observations []domain.UsageObservation
			if err := engine.store.ReadUsage(context.Background(), func(tx *store.UsageReadTx) error {
				var err error
				observations, err = tx.ListRunUsageObservations(context.Background(), run.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(observations) != tc.want {
				t.Fatalf("observations after duplicate collect = %d, want %d", len(observations), tc.want)
			}
		})
	}
}

func TestUsageObservationsPersistWithRoutedReviewFailure(t *testing.T) {
	engine, run, attempt := usageEngineFixture(t, true)
	observedAt := time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)
	measurement := exec.UsageMeasurement{
		Source: domain.UsageSourceReviewSource, Kind: domain.UsageMeasurementReportedUsage,
		Metric: "input_tokens", Unit: "tokens", Quantity: 23,
		Sequence: 1, ObservedAt: observedAt,
	}
	w := &productionPublicationWorkflow{
		store: engine.store, workDir: t.TempDir(), now: func() time.Time { return observedAt },
		reviewRetryAfter: make(map[domain.RunID]time.Time),
	}
	_, err := w.recordReviewSourceFailure(
		context.Background(), productionPublicationTask{RunID: run.ID}, attempt.InvocationID, 1,
		strings.Repeat("a", 40), strings.Repeat("b", 40),
		reviewSourceFailureWithUsage(
			domain.ReviewFailureConfiguration,
			errors.New("engine rejected the reviewed finding"),
			[]exec.UsageMeasurement{measurement},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	var observations []domain.UsageObservation
	if err := engine.store.ReadUsage(context.Background(), func(tx *store.UsageReadTx) error {
		var err error
		observations, err = tx.ListRunUsageObservations(context.Background(), run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].Quantity != measurement.Quantity ||
		observations[0].Source != domain.UsageSourceReviewSource {
		t.Fatalf("failed review usage = %#v, want %#v", observations, measurement)
	}
}

func TestBillableCostSoFarProjectsUsageObservations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	engine, run, attempt := usageEngineFixture(t, true)

	got, err := billableCostSoFar(ctx, engine.store, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("cost before observations = %#v, want nil", got)
	}

	observedAt := time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)
	if err := appendUsageObservations(ctx, engine.store, attempt.InvocationID, []exec.UsageMeasurement{
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementBillableCost,
			Metric: "billable_cost", Unit: "usd_micros", Quantity: 17_500_000,
			Sequence: 1, ObservedAt: observedAt,
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = billableCostSoFar(ctx, engine.store, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := &domain.CostSoFar{Currency: "USD", Amount: "17.5", Invocations: 1, Complete: true}
	if got == nil || *got != *want {
		t.Fatalf("cost = %#v, want %#v", got, want)
	}

	if err := appendUsageObservations(ctx, engine.store, attempt.InvocationID, []exec.UsageMeasurement{
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementBillableCost,
			Metric: "other_billable_cost", Unit: "credits", Quantity: 3,
			Sequence: 2, ObservedAt: observedAt.Add(time.Second),
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = billableCostSoFar(ctx, engine.store, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want.Complete = false
	if got == nil || *got != *want {
		t.Fatalf("cost with unknown unit = %#v, want %#v", got, want)
	}

	unknownEngine, unknownRun, unknownAttempt := usageEngineFixture(t, true)
	if err := appendUsageObservations(ctx, unknownEngine.store, unknownAttempt.InvocationID, []exec.UsageMeasurement{
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementBillableCost,
			Metric: "billable_cost", Unit: "credits", Quantity: 3,
			Sequence: 1, ObservedAt: observedAt,
		},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = billableCostSoFar(ctx, unknownEngine.store, unknownRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	want = &domain.CostSoFar{Currency: "USD", Amount: "0", Invocations: 1, Complete: false}
	if got == nil || *got != *want {
		t.Fatalf("cost with only an unknown unit = %#v, want %#v", got, want)
	}
}
