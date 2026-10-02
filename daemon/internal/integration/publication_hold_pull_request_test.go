package integration_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// These tests cover issue #531: a run held after its pull request exists keeps
// that pull request on its hold item and records a binding the active-resource
// reconciler can observe.

var holdWithPullRequestActions = []domain.Action{
	domain.ActionInspectTrustFailure, domain.ActionOpenPR,
}

func (p *productionPublicationHarness) holdItemID() domain.ItemID {
	return domain.ProductionBlockedItemID(p.runID)
}

func (p *productionPublicationHarness) holdItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	// The snapshot read runs the store's item gate, so a hold whose binding
	// the store cannot prove fails here.
	hold, err := p.attention.GetAttentionItem(p.ctx, p.holdItemID())
	if err != nil {
		t.Fatalf("read hold item: %v", err)
	}
	return hold.Item
}

// publishThenStop publishes the run's pull request and stops before readiness,
// leaving a published run with no ready item.
func (p *productionPublicationHarness) publishThenStop(t *testing.T) {
	t.Helper()
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error {
			return errors.New("stop after publication, before readiness")
		},
	}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("afterPublication seam did not interrupt reconciliation")
	}
}

// assertHoldCarriesPullRequest checks the open hold against the one pull
// request the forge holds: the reference, the open_pr action, and the held
// binding the store proves against the publication records.
func (p *productionPublicationHarness) assertHoldCarriesPullRequest(t *testing.T) domain.AttentionItem {
	t.Helper()
	pulls := p.forge.pullRequests()
	if len(pulls) != 1 {
		t.Fatalf("forge holds %d pull requests, want 1", len(pulls))
	}
	hold := p.holdItem(t)
	want := domain.PRReference{Repo: p.profile.Repo, Number: pulls[0].Number}
	if hold.Type != domain.AttentionPublishBlocked || hold.Status != domain.StatusOpen ||
		hold.PRReference == nil || *hold.PRReference != want ||
		!slices.Equal(hold.RequestedDecision, holdWithPullRequestActions) {
		t.Fatalf("hold item = %#v, want open with reference %#v and open_pr", hold, want)
	}
	var binding domain.HeldItemPRBinding
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		binding, err = tx.GetHeldItemPRBinding(p.ctx, hold.ID)
		return err
	}); err != nil {
		t.Fatalf("read held binding: %v", err)
	}
	if binding.RunID != p.runID || binding.ProducingInvocationID != p.invocation ||
		binding.PublicationInvocationID != domain.ProductionPublicationInvocationID(p.runID) ||
		binding.Repo != want.Repo || binding.RepositoryID != p.profile.RepositoryID ||
		binding.PRNumber != want.Number || binding.HeadSHA != p.replay.HeadSHA {
		t.Fatalf("held binding = %#v, want pull request %#v at head %s", binding, want, p.replay.HeadSHA)
	}
	return hold
}

func (p *productionPublicationHarness) assertHoldWithoutPullRequest(t *testing.T) domain.AttentionItem {
	t.Helper()
	hold := p.holdItem(t)
	if hold.Status != domain.StatusOpen || hold.PRReference != nil ||
		!slices.Equal(hold.RequestedDecision, []domain.Action{domain.ActionInspectTrustFailure}) {
		t.Fatalf("hold item = %#v, want open with no reference and no open_pr", hold)
	}
	err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetHeldItemPRBinding(p.ctx, hold.ID)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("held binding for a hold with no reference: %v, want not found", err)
	}
	return hold
}

// assertHoldSurvivesRestart reconciles the held run again under a fresh
// engine, past the paced retry, and checks the hold restates itself: the same
// item version, reference, and binding, and no lane error.
func (p *productionPublicationHarness) assertHoldSurvivesRestart(
	t *testing.T, approvedRecipes map[domain.Digest]bool,
) {
	t.Helper()
	before := p.assertHoldCarriesPullRequest(t)
	for pass := range 2 {
		p.now = p.now.Add(time.Minute)
		p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, approvedRecipes)
		result, err := p.reconcileLanes()
		if err != nil || result.BlockedItemsCreated != 0 || result.ReadyItemsCreated != 0 ||
			result.PublicationTasksCompleted != 0 {
			t.Fatalf("restart pass %d over a held run = %#v, %v", pass, result, err)
		}
		after := p.assertHoldCarriesPullRequest(t)
		if after.ItemVersion != before.ItemVersion {
			t.Fatalf("restart pass %d moved the hold from version %d to %d",
				pass, before.ItemVersion, after.ItemVersion)
		}
	}
}

func revokedRecipes() map[domain.Digest]bool {
	return map[domain.Digest]bool{productionDigest([]byte("unrelated recipe")): true}
}

func TestHoldOpenBeforePublicationGainsPullRequest(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.conflictNextPush()
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("external-conflict hold = %#v, %v", result, err)
	}
	// No pull request exists yet, so the hold names none.
	if _, prs := p.forge.counts(); prs != 0 {
		t.Fatalf("conflicted publication opened %d pull requests", prs)
	}
	early := p.assertHoldWithoutPullRequest(t)

	// The conflict is repaired and the run publishes, then is held at the
	// recipe re-gate before it reaches readiness.
	p.forge.clearRefs()
	p.now = p.now.Add(time.Minute)
	p.publishThenStop(t)
	p.now = p.now.Add(time.Minute)
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, revokedRecipes())
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("post-publication recipe hold = %#v, %v", result, err)
	}
	held := p.assertHoldCarriesPullRequest(t)
	if held.ID != early.ID || held.ItemVersion != early.ItemVersion+1 {
		t.Fatalf("hold moved from %s v%d to %s v%d, want the same item one version later",
			early.ID, early.ItemVersion, held.ID, held.ItemVersion)
	}
	p.assertHoldSurvivesRestart(t, revokedRecipes())
}

func TestHoldKeepsPullRequestWhenCauseChanges(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.publishThenStop(t)
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, revokedRecipes())
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("post-publication recipe hold = %#v, %v", result, err)
	}
	recipe := p.assertHoldCarriesPullRequest(t)

	// The same hold is restated for an external conflict. A first hold for
	// that cause carries no pull request, but the store refuses a version
	// that drops a reference, so this one keeps it.
	p.transport.failFetch(fmt.Errorf("fetch refused: %w", publish.ErrRemoteMissingBase))
	p.now = p.now.Add(time.Minute)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("external-conflict restatement = %#v, %v", result, err)
	}
	conflict := p.assertHoldCarriesPullRequest(t)
	if conflict.ItemVersion != recipe.ItemVersion+1 ||
		!strings.Contains(conflict.Reason, "external service permanently refused") {
		t.Fatalf("restated hold = %#v, want version %d for the external conflict",
			conflict, recipe.ItemVersion+1)
	}
	p.assertHoldSurvivesRestart(t, map[domain.Digest]bool{p.recipeD: true})

	// And for a third cause, a trust refusal raised before the checkpoint
	// loads.
	p.transport.failFetch(fmt.Errorf("fetch refused: %w", publish.ErrTrustProfileDrift))
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("trust restatement = %#v, %v", result, err)
	}
	trust := p.assertHoldCarriesPullRequest(t)
	if trust.ItemVersion != conflict.ItemVersion+1 {
		t.Fatalf("trust restatement version = %d, want %d", trust.ItemVersion, conflict.ItemVersion+1)
	}
}

func TestExternalConflictFirstHoldCarriesNoPullRequest(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.publishThenStop(t)
	// The run has published, but its first hold is for an external conflict:
	// the recorded pull request is the thing in doubt, so the card names none.
	p.transport.failFetch(fmt.Errorf("fetch refused: %w", publish.ErrRemoteMissingBase))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("external-conflict hold on a published run = %#v, %v", result, err)
	}
	if _, prs := p.forge.counts(); prs != 1 {
		t.Fatalf("published run has %d pull requests, want 1", prs)
	}
	first := p.assertHoldWithoutPullRequest(t)
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 0 {
		t.Fatalf("external-conflict replay = %#v, %v", result, err)
	}
	if replay := p.assertHoldWithoutPullRequest(t); replay.ItemVersion != first.ItemVersion {
		t.Fatalf("external-conflict replay moved the hold to version %d", replay.ItemVersion)
	}
}

func TestPublishedRunHeldBeforeCheckpointCarriesPullRequest(t *testing.T) {
	t.Parallel()
	// A retry pass can hold before the verification checkpoint loads. The run
	// has still published, so the hold carries its pull request.
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.publishThenStop(t)
	p.transport.failFetch(fmt.Errorf("fetch refused: %w", publish.ErrTrustProfileDrift))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("trust hold on a published run = %#v, %v", result, err)
	}
	p.assertHoldCarriesPullRequest(t)
}

func TestHoldBeforePublicationCarriesNoPullRequest(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	// A trust refusal, not an external conflict: the cause would carry a pull
	// request if the run had one.
	p.transport.failFetch(fmt.Errorf("fetch refused: %w", publish.ErrTrustProfileDrift))
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("pre-publication hold = %#v, %v", result, err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("pre-publication hold caused effects: %d refs/%d PRs", refs, prs)
	}
	first := p.assertHoldWithoutPullRequest(t)
	p.now = p.now.Add(time.Minute)
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 0 {
		t.Fatalf("pre-publication hold replay = %#v, %v", result, err)
	}
	if replay := p.assertHoldWithoutPullRequest(t); replay.ItemVersion != first.ItemVersion {
		t.Fatalf("pre-publication hold replay moved the hold to version %d", replay.ItemVersion)
	}
}

func TestReviewRecordHoldRecoversToReadinessWithItsPullRequest(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.publishThenStop(t)
	// The daemon's reviewer configuration drifts away from the one the run's
	// clean record was produced under, so the published run is held.
	approved := p.reviewConfigurationDigest
	p.reviewConfigurationDigest = domain.Digest("sha256:" + strings.Repeat("d", 64))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("review-record hold = %#v, %v", result, err)
	}
	held := p.assertHoldCarriesPullRequest(t)

	// Restoring the configuration recovers the run to readiness.
	p.reviewConfigurationDigest = approved
	p.now = p.now.Add(time.Minute)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	result, err = p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 {
		t.Fatalf("review-record recovery = %#v, %v", result, err)
	}
	p.assertReady(t)
	superseded := p.holdItem(t)
	if superseded.Status != domain.StatusSuperseded || superseded.PRReference == nil ||
		*superseded.PRReference != *held.PRReference {
		t.Fatalf("recovered hold = %#v, want superseded with reference %#v", superseded, *held.PRReference)
	}
	var (
		ready   domain.AttentionItem
		binding domain.ReadyItemPRBinding
		watches int
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		ready, err = tx.GetAttentionItem(p.ctx, domain.ProductionReadyItemID(p.runID))
		if err != nil {
			return err
		}
		binding, err = tx.GetReadyItemPRBinding(p.ctx, ready.ID)
		if err != nil {
			return err
		}
		for _, kind := range []domain.ScheduleKind{
			domain.SchedulePRChecksDeadline, domain.ScheduleReviewWaitThreshold, domain.ScheduleBaseAdvanceWatch,
		} {
			_, err := tx.GetSchedule(p.ctx, engine.PublicationWatchScheduleID(kind, ready.ID))
			switch {
			case err == nil:
				watches++
			case !errors.Is(err, store.ErrNotFound):
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ready.PRReference == nil || *ready.PRReference != *held.PRReference ||
		binding.PRNumber != held.PRReference.Number || binding.Repo != held.PRReference.Repo {
		t.Fatalf("ready item %#v and binding %#v, want pull request %#v",
			ready.PRReference, binding, *held.PRReference)
	}
	if watches == 0 {
		t.Fatal("recovered readiness armed no publication watch")
	}
}

// holdPublishedSuccessor publishes the sealed successor, holds it at the
// recipe re-gate before readiness, and checks the successor's own hold item
// carries the pull request. It leaves an approving engine in place so the
// caller's convergence loop recovers the successor to readiness, which also
// runs signet's conclusion check over the superseded hold.
func holdPublishedSuccessor(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterPublication: func() error {
			return errors.New("stop after successor publication, before readiness")
		},
	}, true)
	var stopped error
	for range 4 {
		if _, stopped = p.reconcileLanes(); stopped != nil {
			break
		}
	}
	if stopped == nil || !strings.Contains(stopped.Error(), "before readiness") {
		t.Fatalf("successor publication seam did not interrupt reconciliation: %v", stopped)
	}
	p.now = p.now.Add(time.Minute)
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, revokedRecipes())
	for pass := range 2 {
		result, err := p.reconcileLanes()
		if err != nil || result.ReadyItemsCreated != 0 || result.PublicationTasksCompleted != 0 {
			t.Fatalf("held successor pass %d = %#v, %v", pass, result, err)
		}
		p.now = p.now.Add(time.Minute)
	}
	pulls := p.forge.pullRequests()
	if len(pulls) != 1 {
		t.Fatalf("forge holds %d pull requests, want 1", len(pulls))
	}
	want := domain.PRReference{Repo: p.profile.Repo, Number: pulls[0].Number}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil || successor == nil {
			return fmt.Errorf("no sealed successor: %w", err)
		}
		if successor.BlockedItemID() == domain.ProductionBlockedItemID(p.runID) {
			t.Fatal("successor hold shares the original run's hold item")
		}
		hold, err := tx.GetAttentionItem(p.ctx, successor.BlockedItemID())
		if err != nil {
			return err
		}
		if hold.Status != domain.StatusOpen || hold.PRReference == nil || *hold.PRReference != want ||
			!slices.Equal(hold.RequestedDecision, holdWithPullRequestActions) {
			t.Fatalf("successor hold = %#v, want open with reference %#v and open_pr", hold, want)
		}
		binding, err := tx.GetHeldItemPRBinding(p.ctx, hold.ID)
		if err != nil {
			return err
		}
		if binding.RunID != p.runID || binding.PRNumber != want.Number || binding.Repo != want.Repo ||
			binding.HeadSHA != p.replay.HeadSHA || binding.HeadSHA != hold.PRHeadSHA {
			t.Fatalf("successor held binding = %#v, want pull request %#v at head %s",
				binding, want, p.replay.HeadSHA)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
}

func TestHoldRefusesADifferentRecordedPullRequest(t *testing.T) {
	t.Parallel()
	// The pull request number is read back from a stored outcome row. If that
	// row later names another pull request, the hold must not follow it: the
	// item's reference is immutable and its binding is write-once, so the pass
	// fails loudly and changes nothing.
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.publishThenStop(t)
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, revokedRecipes())
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("post-publication recipe hold = %#v, %v", result, err)
	}
	held := p.assertHoldCarriesPullRequest(t)

	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var key string
	var payload []byte
	if err := raw.QueryRowContext(p.ctx,
		`SELECT idempotency_key, payload FROM inbox WHERE kind = 'publish.outcome'`,
	).Scan(&key, &payload); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	var outcome map[string]any
	if err := json.Unmarshal(payload, &outcome); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	outcome["pr_number"] = held.PRReference.Number + 1
	forged, err := json.Marshal(outcome)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	_, updateErr := raw.ExecContext(p.ctx,
		`UPDATE inbox SET payload = ? WHERE idempotency_key = ?`, forged, key)
	if closeErr := raw.Close(); updateErr != nil || closeErr != nil {
		t.Fatal(errors.Join(updateErr, closeErr))
	}

	p.now = p.now.Add(time.Minute)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("hold followed a recorded pull request that changed under it")
	}
	var after domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		// The record tier: the store's item gate now refuses the snapshot
		// read, because the binding no longer matches the outcome row.
		var err error
		after, err = tx.GetAttentionItemRecord(p.ctx, held.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.ItemVersion != held.ItemVersion || after.PRReference == nil ||
		*after.PRReference != *held.PRReference {
		t.Fatalf("hold after a forged outcome = %#v, want unchanged %#v", after, held)
	}
}

// A declared run held after publication records its work-unit binding from
// the hold, so the reconciler can complete the unit while the run stays held,
// and the readiness that follows restates the same record.
func TestDeclaredRunHeldAfterPublicationRecordsWorkUnitBinding(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 443)
	p.startAndRecordExport(t)
	p.publishThenStop(t)
	readBinding := func() (domain.WorkUnitPRBinding, error) {
		var binding domain.WorkUnitPRBinding
		err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			binding, err = tx.GetWorkUnitPRBinding(p.ctx, p.declaration.ID)
			return err
		})
		return binding, err
	}
	if _, err := readBinding(); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("work-unit binding before the hold = %v, want ErrNotFound", err)
	}

	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true, revokedRecipes())
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("post-publication recipe hold = %#v, %v", result, err)
	}
	held := p.assertHoldCarriesPullRequest(t)
	binding, err := readBinding()
	if err != nil {
		t.Fatalf("read work-unit binding: %v", err)
	}
	if binding.Repo != held.PRReference.Repo || binding.PRNumber != held.PRReference.Number ||
		binding.RepositoryID != p.profile.RepositoryID || binding.HeadSHA != held.PRHeadSHA ||
		binding.BaseRef == "" {
		t.Fatalf("work-unit binding = %+v, want the held pull request's coordinates", binding)
	}

	p.now = p.now.Add(time.Minute)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
	if again, err := readBinding(); err != nil || again != binding {
		t.Fatalf("readiness restated the binding as %+v, %v; want %+v", again, err, binding)
	}
}
