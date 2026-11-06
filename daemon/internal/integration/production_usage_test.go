package integration_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
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
		AuthStoreMutationLease: true, Enabled: true, CostOwner: "operator",
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
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		if err := storetest.RecordIdentityOnPool(p.ctx, tx, identity, testPoolLimit, fakePublicationTime.Add(-4*time.Minute)); err != nil {
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

// loopbackClaudeUsageAt parses the pinned 2.1.282 loopback turn's usage
// capture, observed at the export stamp a driver pins it to.
func loopbackClaudeUsageAt(t *testing.T, observedAt time.Time) *exec.ClaudeUsageCapture {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "exec", "claude", "testdata", "claude_usage_loopback_turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	capture := claude.ExtractUsageCapture(body, observedAt)
	if capture == nil {
		t.Fatal("the loopback fixture carries no usage capture")
	}
	return capture
}

// countingUsageCollector counts the collections that reach the collector.
type countingUsageCollector struct {
	ward.UsageCollector
	calls atomic.Int32
}

func (c *countingUsageCollector) CollectUsage(
	ctx context.Context, request ward.UsageCollectionRequest,
) (ward.UsageCollection, error) {
	c.calls.Add(1)
	return c.UsageCollector.CollectUsage(ctx, request)
}

// deliveringClaudeUsageSink is the daemon's sink shape over the real
// collector and coordinator (cmd/freesided claudeUsageSink), and also records
// the pool of every capture handed to it.
type deliveringClaudeUsageSink struct {
	collector *ward.ClaudeUsageCollector
	coord     *usagecollect.Coordinator
	mu        sync.Mutex
	pools     []domain.UsagePoolID
	// kept counts the captures the collector kept, each of which starts a
	// collection.
	kept int
}

func (s *deliveringClaudeUsageSink) ClaudeUsageCaptured(
	ctx context.Context, pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID, generation int,
	capture exec.ClaudeUsageCapture,
) error {
	s.mu.Lock()
	s.pools = append(s.pools, pool)
	s.mu.Unlock()
	if !s.collector.Record(enrollment, generation, capture) {
		return nil
	}
	s.mu.Lock()
	s.kept++
	s.mu.Unlock()
	return s.coord.CaptureArrived(ctx, pool, enrollment, generation)
}

func (s *deliveringClaudeUsageSink) delivered() ([]domain.UsagePoolID, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pools), s.kept
}

// TestUnattendedExportDeliversClaudeUsage is #2716's case. An unattended
// implementation export mints its publication task and is never collected,
// so its writer turn's Claude usage capture reaches the collector when the
// export is recorded, and the pool's collection then answers with the
// capture's windows instead of failing for want of a writer turn. The
// identical crash-window re-record keeps nothing newer and starts no
// collection, and the publication lane, replaying from the decoded task,
// reports nothing.
func TestUnattendedExportDeliversClaudeUsage(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	bindProductionWriters(t, p)
	collector := ward.NewClaudeUsageCollector()
	counted := &countingUsageCollector{UsageCollector: collector}
	now := fakePublicationTime.Add(time.Minute)
	coord := usagecollect.New(p.store, map[domain.HarnessClientKind]ward.UsageCollector{
		domain.HarnessClientClaudeCode: counted,
	}, usagecollect.Config{Now: func() time.Time { return now }})
	ctx, cancel := context.WithCancel(p.ctx)
	coord.Start(ctx)
	t.Cleanup(func() {
		cancel()
		coord.Wait()
	})
	sink := &deliveringClaudeUsageSink{collector: collector, coord: coord}
	// The engine reports collected terminals to the same sink, as the
	// daemon's does, so a report from the publication lane would show.
	p.workflow = p.newEngineForMode(t, productionCrashSeams{}, true,
		map[domain.Digest]bool{p.recipeD: true}, domain.ModeUnattended, false,
		engine.WithClaudeUsageSink(sink))
	reviewID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(reviewID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			CompletedAt:        p.now.UTC(),
			CompletionEvidence: productionDigest([]byte("clean review")),
		},
	})

	record := p.startExecutionExport(t, p.replay.HeadSHA)
	replay := p.replay
	replay.ClaudeUsage = loopbackClaudeUsageAt(t, record.RecordedAt)
	if err := engine.RecordExecutionExport(p.ctx, p.store, record, replay, sink, p.logger); err != nil {
		t.Fatalf("record export: %v", err)
	}
	pools, kept := sink.delivered()
	if len(pools) != 1 || kept != 1 {
		t.Fatalf("captures delivered at export = %v, kept %d; want one, kept", pools, kept)
	}
	pool := pools[0]
	report, err := coord.Refresh(ctx, pool)
	if err != nil || report.Outcome != usagecollect.OutcomeCollected {
		t.Fatalf("refresh after the export = %+v, %v; want collected", report, err)
	}
	var observation domain.PoolUsageObservation
	readObservation := func() {
		t.Helper()
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			observation, err = tx.GetPoolUsageObservation(p.ctx, pool)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	readObservation()
	shares := map[string]int{}
	for _, window := range replay.ClaudeUsage.Windows {
		shares[window.Window] = window.UsedPercent
	}
	if len(observation.Windows) != 2 {
		t.Fatalf("windows = %+v, want five_hour and seven_day", observation.Windows)
	}
	for _, reading := range observation.Windows {
		share, ok := shares[reading.Window]
		if !ok || reading.UsedPercent == nil || *reading.UsedPercent != share {
			t.Fatalf("reading %+v, want the answer's share of %s", reading, reading.Window)
		}
	}
	collections := counted.calls.Load()

	// The crash-window re-record converges, and the collector keeps nothing
	// newer, so it starts no collection.
	if err := engine.RecordExecutionExport(p.ctx, p.store, record, replay, sink, p.logger); err != nil {
		t.Fatalf("re-record export: %v", err)
	}
	if got, kept := sink.delivered(); len(got) != 2 || kept != 1 {
		t.Fatalf("captures delivered after the re-record = %v, kept %d; want the same one again, not kept", got, kept)
	}
	// The publication lane replays from the decoded task, which carries no
	// capture, and never collects the export's terminal.
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("publish: %v", err)
	}
	p.assertReady(t)
	if got, kept := sink.delivered(); len(got) != 2 || kept != 1 {
		t.Fatalf("captures delivered after publication = %v, kept %d; want none from the lane", got, kept)
	}
	cancel()
	coord.Wait()
	if got := counted.calls.Load(); got != collections {
		t.Fatalf("collections = %d, want %d: a replayed export started one", got, collections)
	}
	before := observation
	readObservation()
	if !reflect.DeepEqual(observation, before) {
		t.Fatalf("record after the replays = %+v, want %+v", observation, before)
	}
}

// TestUnattendedExportWithoutAClaudeUsageSinkWarns pins that a production
// export's capture dropped for want of a sink leaves a warning naming the
// invocation, and the export still records.
func TestUnattendedExportWithoutAClaudeUsageSinkWarns(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	bindProductionWriters(t, p)
	record := p.startExecutionExport(t, p.replay.HeadSHA)
	replay := p.replay
	replay.ClaudeUsage = loopbackClaudeUsageAt(t, record.RecordedAt)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	if err := engine.RecordExecutionExport(p.ctx, p.store, record, replay, nil, logger); err != nil {
		t.Fatalf("record export: %v", err)
	}
	var warnings []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "level=WARN") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "invocation="+string(p.invocation)) {
		t.Fatalf("warnings = %q, want one naming invocation %s", warnings, p.invocation)
	}
}
