package integration_test

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestPublishedRunRecordsWriterAndReviewUsage is #2212's end-to-end case. An
// unattended export mints its publication task and is never collected, so the
// writer's usage lands in the export's own write, and the identical
// crash-window retry converges on it. The review is admitted into
// review_admissions, and its usage lands against that admission. Both count
// toward the run's billable cost.
func TestPublishedRunRecordsWriterAndReviewUsage(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	bindProductionWriters(t, p)
	observedAt := time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)
	cost := func(source domain.UsageSource, micros int64) exec.UsageMeasurement {
		return exec.UsageMeasurement{
			Source: source, Kind: domain.UsageMeasurementBillableCost,
			Metric: "total_cost", Unit: "usd_micros", Quantity: micros,
			Sequence: 1, ObservedAt: observedAt,
		}
	}
	reviewID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(reviewID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			CompletedAt:        p.now.UTC(),
			CompletionEvidence: productionDigest([]byte("clean review")),
			Usage:              []exec.UsageMeasurement{cost(domain.UsageSourceReviewSource, 250_000)},
		},
	})

	record := p.startExecutionExport(t, p.replay.HeadSHA)
	replay := p.replay
	replay.Usage = []exec.UsageMeasurement{cost(domain.UsageSourceAdapterTranscript, 1_445_768)}
	// The second call is the crash-window retry the driver makes with the
	// identical export, replay, and pinned usage; it converges.
	for attempt := range 2 {
		if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, replay); err != nil {
			t.Fatalf("record export attempt %d: %v", attempt, err)
		}
	}
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	p.assertReady(t)
	got := runUsageByInvocation(t, p)
	if len(got[p.invocation]) != 1 || got[p.invocation][0].Quantity != 1_445_768 {
		t.Fatalf("writer usage = %#v", got)
	}
	if len(got[reviewID]) != 1 || got[reviewID][0].Quantity != 250_000 {
		t.Fatalf("review usage = %#v", got)
	}
	var billable *domain.CostSoFar
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		billable, err = tx.BillableCostSoFar(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if billable == nil || billable.Amount != "1.695768" || billable.Invocations != 2 || !billable.Complete {
		t.Fatalf("billable cost = %#v, want the writer's and the review's 1.695768, complete", billable)
	}
}

func runUsageByInvocation(
	t *testing.T, p *productionPublicationHarness,
) map[domain.InvocationID][]domain.UsageObservation {
	t.Helper()
	var observations []domain.UsageObservation
	if err := p.store.ReadUsage(p.ctx, func(tx *store.UsageReadTx) error {
		var err error
		observations, err = tx.ListRunUsageObservations(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	byInvocation := map[domain.InvocationID][]domain.UsageObservation{}
	for _, observation := range observations {
		id := domain.InvocationID(observation.InvocationID)
		byInvocation[id] = append(byInvocation[id], observation)
	}
	return byInvocation
}

// bindProductionWriters adopts a Claude identity's claude_code enrollment and
// rebuilds the engine to admit writers through the baseline lineup, so the
// writer's admission carries the agent binding its usage is attributed from.
func bindProductionWriters(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	identity := domain.AuthIdentity{
		ID: "auth-claude-writer", Provider: "claude", AccountBinding: "acct-writer-0001",
		AuthStoreMutationLease: true, MaxParallelExecutions: 64, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "claude-writer-interim", RefreshStrategy: domain.RefreshExternal,
		},
	}
	enrollment := domain.ClientEnrollment{
		ID: "auth-claude-writer/claude_code", AuthIdentityID: identity.ID,
		HarnessClient: domain.HarnessClientClaudeCode, Route: "anthropic-subscription",
		AuthMethod: domain.AuthMethodSetupToken, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshExternal, AccountBinding: identity.AccountBinding,
	}
	manifest := productionDigest([]byte("writer auth store"))
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(p.ctx, identity, fakePublicationTime.Add(-4*time.Minute)); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(p.ctx, enrollment, fakePublicationTime.Add(-3*time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(p.ctx, identity.ID, "inv-adopt-writer",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, Generation: 0,
				AuthStoreVolume: "claude-writer-auth", StoreManifestDigest: manifest,
			}, fakePublicationTime.Add(-2*time.Minute), fakePublicationTime.Add(10*time.Minute))
		if err != nil {
			return err
		}
		if _, err := tx.AppendEnrollmentGeneration(p.ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: "claude-writer-auth",
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			AccountBinding: identity.AccountBinding, RecordedAt: fakePublicationTime.Add(-time.Minute),
		}, fakePublicationTime.Add(-time.Minute)); err != nil {
			return err
		}
		adapter, err := agentbaseline.ClaudeWardAdapter()
		if err != nil {
			return err
		}
		record, err := domain.NewAdapterConformance(domain.AdapterConformanceInput{
			AdapterDigest: adapter.Digest, Outcome: domain.ConformancePassed,
			ProvedCapabilities: domain.NewLaunchCapabilitySet(domain.LaunchCapReadTools, domain.LaunchCapMutationTools,
				domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput, domain.LaunchCapContextSeverance, domain.LaunchCapRouteStoreContract),
			ProvedAt: fakePublicationTime.Add(-time.Minute),
		})
		if err != nil {
			return err
		}
		_, err = tx.RecordAdapterConformance(p.ctx, record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	model := agentbaseline.Selection{Model: "claude-writer-test", Effort: domain.EffortHigh}
	tree, err := agentbaseline.Tree(agentbaseline.TreeInput{
		ClaudeEnrollment: enrollment,
		Writers: map[domain.RoleName]agentbaseline.Selection{
			domain.RoleSpecifier: model, domain.RoleImplementer: model, domain.RoleRemediator: model,
		},
		TermsBasisDate: "2026-07-01", PricingRevision: "pricing-2026-07",
		OfferNotAfter: fakePublicationTime.Add(30 * 24 * time.Hour),
		Prompts: map[domain.RoleName]agentbaseline.Prompt{
			domain.RoleImplementer: {Name: "implementer", Digest: productionDigest([]byte("prompt package"))},
			domain.RoleRemediator:  {Name: "remediator", Digest: p.remediationPromptPackage},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := agenttree.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := agenttree.Revision(files)
	if err != nil {
		t.Fatal(err)
	}
	if tree, err = agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
	p.agents = &engine.AgentSelection{
		Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
		EffectiveEgress: []string{"api.anthropic.com:443"}, AttemptBudget: time.Hour, ExpiryMargin: 5 * time.Minute,
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
}
