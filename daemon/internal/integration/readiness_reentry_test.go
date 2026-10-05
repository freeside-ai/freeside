package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// advanceBase commits one file to the target base and returns the new tip,
// the way a merge to the default branch moves it.
func (p *productionPublicationHarness) advanceBase(t *testing.T, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.baseDir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, p.baseDir, "add", "-A")
	runGit(t, p.baseDir, "commit", "-q", "-m", "advance "+name)
	return runGit(t, p.baseDir, "rev-parse", "HEAD")
}

// pushToPullRequest commits one file on top of the pull request's head and
// pushes it to the branch, the way a person's push moves it. It returns the
// new head.
func (p *productionPublicationHarness) pushToPullRequest(t *testing.T, name, content string) string {
	t.Helper()
	return p.pushCommitToPullRequest(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	})
}

// pushCommitToPullRequest commits whatever change makes to a checkout of the
// pull request's branch and pushes it, as anyone with push access can.
func (p *productionPublicationHarness) pushCommitToPullRequest(t *testing.T, change func(dir string)) string {
	t.Helper()
	pull := p.forge.pullRequests()[0]
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "fetch", "-q", p.transport.headRepo(), "refs/heads/"+pull.HeadRef)
	runGit(t, dir, "checkout", "-q", "--detach", "FETCH_HEAD")
	change(dir)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "push to the pull request")
	head := runGit(t, dir, "rev-parse", "HEAD")
	p.transport.pushBranchHead(dir, pull.HeadRef, head)
	return head
}

// currentReadyItem returns the ready item of the run's newest cycle.
func (p *productionPublicationHarness) currentReadyItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		id, err := tx.CurrentProductionReadyItemID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		item, err = tx.GetAttentionItem(p.ctx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// invalidateReady supersedes the run's current ready item with fact and
// starts the re-entry in that transaction, as both daemon writers do. It
// reports whether a cycle started.
func (p *productionPublicationHarness) invalidateReady(
	t *testing.T, fact domain.ReadinessInvalidation,
) (domain.AttentionItem, bool) {
	t.Helper()
	item := p.currentReadyItem(t)
	if item.Status != domain.StatusOpen {
		t.Fatalf("ready item %s is %s, want open", item.ID, item.Status)
	}
	fact.ObservedAt = p.now.UTC()
	item.ReadinessInvalidation = &fact
	if fact.Reason == domain.ReadinessInvalidationBaseAdvanced {
		item.BaseFreshness = &domain.BaseFreshness{
			BaseRef: "main", AdmittedBaseSHA: fact.Bound, ObservedBaseSHA: fact.Observed,
			Advanced: true, ObservedAt: fact.ObservedAt,
		}
	}
	item.Status = domain.StatusSuperseded
	item.ItemVersion++
	started := false
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutAttentionItem(p.ctx, item); err != nil {
			return err
		}
		var err error
		started, err = engine.StartReadinessReentry(p.ctx, tx, item)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item, started
}

// scriptCleanReview scripts a clean pass for one review round.
func (p *productionPublicationHarness) scriptCleanReview(round int, baseSHA, headSHA string) {
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, round), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: baseSHA, HeadSHA: headSHA,
			Provider: "openai", ModelConfiguration: "codex/test",
			CostOwner: "test", CompletedAt: p.now.UTC(),
			CompletionEvidence: productionDigest([]byte("clean re-entry review")),
		},
	})
}

// reviewedTree records, for each review request, the request and the files
// the reviewer was handed, read at request time.
type reviewedTree struct {
	// The fault source forwards the optional interfaces the engine probes
	// for, which a bare embedded exec.ReviewSource would hide.
	*faultReviewSource
	t        *testing.T
	requests []exec.ReviewRequest
	files    []map[string]string
}

func (r *reviewedTree) RequestReview(
	ctx context.Context, id domain.InvocationID, req exec.ReviewRequest,
) error {
	files := map[string]string{}
	for _, name := range []string{"README.md", "UPSTREAM.md", "FOLLOWUP.md"} {
		body, err := os.ReadFile(filepath.Join(req.Workspace, name)) //nolint:gosec // G304: fixed names in the test-owned review workspace.
		if err == nil {
			files[name] = string(body)
		} else if !errors.Is(err, os.ErrNotExist) {
			r.t.Errorf("read review workspace %s: %v", name, err)
		}
	}
	r.requests, r.files = append(r.requests, req), append(r.files, files)
	return r.faultReviewSource.RequestReview(ctx, id, req)
}

// recordReviews rebuilds the engine with a review source that records what
// each later round is handed.
func (p *productionPublicationHarness) recordReviews(t *testing.T) *reviewedTree {
	t.Helper()
	recorded := &reviewedTree{faultReviewSource: &faultReviewSource{ReviewSource: p.reviewer}, t: t}
	p.reviewSource = recorded
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return recorded
}

// publishReady drives the first cycle to its open ready item.
func (p *productionPublicationHarness) publishReady(t *testing.T) domain.AttentionItem {
	t.Helper()
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("first cycle = %#v, %v", result, err)
	}
	return p.currentReadyItem(t)
}

// assertReentered checks the durable end state of one clean re-entry: a new
// open ready item bound in place to the same pull request, a clean review of
// exactly the cycle's coordinates, and nothing pushed.
func (p *productionPublicationHarness) assertReentered(
	t *testing.T, predecessor domain.AttentionItem, round int, baseSHA, headSHA string,
) domain.AttentionItem {
	t.Helper()
	ready := p.currentReadyItem(t)
	if ready.ID == predecessor.ID || ready.Status != domain.StatusOpen ||
		ready.Type != domain.AttentionReadyForFinalReview || ready.PRHeadSHA != headSHA ||
		ready.ReadinessInvalidation != nil {
		t.Fatalf("re-entered ready item = %#v", ready)
	}
	var (
		binding, prior domain.ReadyItemPRBinding
		review         domain.ReviewRecord
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if binding, err = tx.GetReadyItemPRBinding(p.ctx, ready.ID); err != nil {
			return err
		}
		if prior, err = tx.GetReadyItemPRBinding(p.ctx, predecessor.ID); err != nil {
			return err
		}
		review, err = tx.LatestReviewRecord(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if binding.PRNumber != prior.PRNumber || binding.RepositoryID != prior.RepositoryID ||
		binding.ProducingInvocationID != prior.ProducingInvocationID ||
		binding.HeadSHA != headSHA || binding.RunID != p.runID {
		t.Fatalf("re-entered binding = %#v, predecessor %#v", binding, prior)
	}
	if review.Round != round || review.Outcome != domain.ReviewClean ||
		review.BaseSHA != baseSHA || review.HeadSHA != headSHA {
		t.Fatalf("re-entry review = %#v, want clean round %d of %s..%s",
			review, round, baseSHA, headSHA)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("re-entry changed publication effects: %d refs, %d PRs", refs, prs)
	}
	return ready
}

func TestReadinessReentersAfterBaseAdvance(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	first := p.publishReady(t)
	pushes, body := p.transport.pushCount(), p.forge.pullRequests()[0].Body

	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	})
	if !started {
		t.Fatal("a base advance started no re-entry")
	}
	reviews := p.recordReviews(t)
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadyItemsCreated != 1 || result.PublicationTasksCompleted != 1 ||
		result.BlockedItemsCreated != 0 {
		t.Fatalf("re-entry result = %#v", result)
	}
	// The reviewer is handed the merge of the head into the advanced base,
	// named as the evaluated commit, while the request still binds the head
	// the forge shows.
	if len(reviews.requests) != 1 {
		t.Fatalf("review requests = %d, want 1", len(reviews.requests))
	}
	request, tree := reviews.requests[0], reviews.files[0]
	if request.Round != 2 || request.BaseSHA != advanced || request.HeadSHA != p.replay.HeadSHA ||
		request.EvaluatedSHA == "" || request.EvaluatedSHA == p.replay.HeadSHA ||
		request.EvaluatedSHA == advanced {
		t.Fatalf("review request = %#v", request)
	}
	if tree["README.md"] != "production change\n" || tree["UPSTREAM.md"] != "upstream change\n" {
		t.Fatalf("reviewed tree = %#v, want the head's change on the advanced base", tree)
	}
	p.assertReentered(t, superseded, 2, advanced, p.replay.HeadSHA)
	if first.ID != superseded.ID {
		t.Fatalf("superseded %s, want the first ready item %s", superseded.ID, first.ID)
	}
	if p.room.runs != 2 || p.transport.mergeCount() != 1 {
		t.Fatalf("verification runs = %d, merges = %d, want 2 and 1",
			p.room.runs, p.transport.mergeCount())
	}
	if p.transport.pushCount() != pushes || p.forge.pullRequests()[0].Body != body {
		t.Fatal("re-entry pushed or rewrote the pull request")
	}
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
	if p.room.runs != 2 {
		t.Fatalf("converged replay re-ran verification: %d runs", p.room.runs)
	}
}

func TestReadinessReentersAfterHeadChange(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	head := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: head,
	})
	if !started {
		t.Fatal("a head change started no re-entry")
	}
	reviews := p.recordReviews(t)
	p.scriptCleanReview(2, p.baseSHA, head)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("re-entry result = %#v, %v", result, err)
	}
	p.assertReentered(t, superseded, 2, p.baseSHA, head)
	// A head change reviews the pushed head itself against the prior base.
	if len(reviews.requests) != 1 || reviews.requests[0].EvaluatedSHA != "" ||
		reviews.requests[0].BaseSHA != p.baseSHA || reviews.requests[0].HeadSHA != head ||
		reviews.files[0]["FOLLOWUP.md"] != "pushed after readiness\n" {
		t.Fatalf("review request = %#v, tree %#v", reviews.requests, reviews.files)
	}
	if p.room.runs != 2 || p.transport.mergeCount() != 0 {
		t.Fatalf("verification runs = %d, merges = %d, want 2 and 0",
			p.room.runs, p.transport.mergeCount())
	}
}

func TestReadinessReentryStopsOnMergeConflict(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "README.md", "conflicting upstream change\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	})
	if !started {
		t.Fatal("a base advance started no re-entry")
	}
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("conflict result = %#v", result)
	}
	var stop domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		stop, err = tx.GetAttentionItem(p.ctx, successor.BlockedItemID())
		if err != nil {
			return err
		}
		if successor.PredecessorItemID != superseded.ID {
			t.Errorf("cycle predecessor = %s, want %s", successor.PredecessorItemID, superseded.ID)
		}
		if _, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a conflicted cycle wrote a ready item: %v", err)
		}
		if hold, held, err := tx.GetRunHold(p.ctx, p.runID); err != nil || held {
			t.Errorf("a stopped cycle left run hold %#v, %v", hold, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stop.Status != domain.StatusOpen || stop.Type != domain.AttentionPublishBlocked ||
		!strings.Contains(stop.Reason, "README.md") ||
		!strings.Contains(stop.Reason, advanced) {
		t.Fatalf("stop item = %#v", stop)
	}
	if p.room.runs != 1 {
		t.Fatalf("a conflicted cycle ran verification: %d runs", p.room.runs)
	}
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 0 {
		t.Fatalf("stopped cycle replayed = %#v, %v", result, err)
	}
}

// baseWatchBase returns the base an armed base watch on itemID was admitted
// against.
func (p *productionPublicationHarness) baseWatchBase(t *testing.T, itemID domain.ItemID) string {
	t.Helper()
	var bases []string
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		schedules, err := tx.ListSchedules(p.ctx)
		if err != nil {
			return err
		}
		for _, snapshot := range schedules {
			schedule := snapshot.Value
			if schedule.BaseWatch == nil || schedule.Subject.ItemID == nil ||
				*schedule.Subject.ItemID != itemID || schedule.Status != domain.ScheduleArmed {
				continue
			}
			bases = append(bases, schedule.BaseWatch.AdmittedBaseSHA)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(bases) != 1 {
		t.Fatalf("armed base watches on %s = %v, want one", itemID, bases)
	}
	return bases[0]
}

// reconcileUntilReady runs lane passes until the run's current ready item is
// open, and fails after maxPasses.
func (p *productionPublicationHarness) reconcileUntilReady(t *testing.T, maxPasses int) {
	t.Helper()
	for pass := 1; pass <= maxPasses; pass++ {
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("pass %d/%d: %v", pass, maxPasses, err)
		}
		var open bool
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			id, err := tx.CurrentProductionReadyItemID(p.ctx, p.runID)
			if err != nil {
				return err
			}
			item, err := tx.GetAttentionItem(p.ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			open = item.Status == domain.StatusOpen
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if open {
			return
		}
	}
	t.Fatalf("no open ready item within %d passes", maxPasses)
}

// TestReadinessReentersThreeTimesAndWatchesTheCycleBase: the re-entered item's base
// watch is admitted against the cycle's base, so it stays quiet until the
// base moves again, and three advances in a row each re-enter from the item
// the previous cycle produced.
func TestReadinessReentersThreeTimesAndWatchesTheCycleBase(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	first := p.publishReady(t)
	if got := p.baseWatchBase(t, first.ID); got != p.baseSHA {
		t.Fatalf("first watch base = %s, want admission base %s", got, p.baseSHA)
	}
	bound := p.baseSHA
	for cycle := 1; cycle <= 3; cycle++ {
		advanced := p.advanceBase(t, fmt.Sprintf("UPSTREAM-%d.md", cycle), "upstream change\n")
		superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: bound, Observed: advanced,
		})
		if !started {
			t.Fatalf("advance %d started no re-entry", cycle)
		}
		p.scriptCleanReview(cycle+1, advanced, p.replay.HeadSHA)
		if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
			t.Fatalf("advance %d result = %#v, %v", cycle, result, err)
		}
		ready := p.assertReentered(t, superseded, cycle+1, advanced, p.replay.HeadSHA)
		if got := p.baseWatchBase(t, ready.ID); got != advanced {
			t.Fatalf("advance %d watch base = %s, want cycle base %s", cycle, got, advanced)
		}
		bound = advanced
	}
	if p.room.runs != 4 || p.transport.mergeCount() != 3 {
		t.Fatalf("verification runs = %d, merges = %d, want 4 and 3",
			p.room.runs, p.transport.mergeCount())
	}
}

// TestReadinessReentryConvergesAcrossRestarts interrupts a base-advance cycle
// at each durable boundary it crosses, restarts, and requires the same end
// state with one verification and one review request per boundary crossed.
func TestReadinessReentryConvergesAcrossRestarts(t *testing.T) {
	t.Parallel()
	for _, transition := range []engine.DurableTransition{
		engine.DurableTransitionVerificationEvidence,
		engine.DurableTransitionReviewRequest,
		engine.DurableTransitionReviewResult,
		engine.DurableTransitionReadyItem,
	} {
		for _, side := range engine.AllDurableTransitionSides {
			t.Run(string(transition)+"/"+string(side), func(t *testing.T) {
				t.Parallel()
				p := newProductionPublicationHarness(t, "")
				p.publishReady(t)
				advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
				superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
					Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
				})
				if !started {
					t.Fatal("a base advance started no re-entry")
				}
				// A restart between the invalidation and the first pass of
				// the cycle: the task row alone carries it.
				p.restartDurableState(t)
				reviewCalls := &faultReviewSource{ReviewSource: p.reviewer}
				p.reviewSource = reviewCalls
				p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
				injected := false
				p.workflow = p.newEngine(t, productionCrashSeams{
					transitionHook: func(
						got engine.DurableTransition, observed engine.DurableTransitionSide,
					) error {
						if !injected && got == transition && observed == side {
							injected = true
							return errors.New("injected process loss")
						}
						return nil
					},
				}, true)
				if _, err := p.reconcileLanes(); err == nil || !injected {
					t.Fatalf("%s/%s did not interrupt the cycle: %v", transition, side, err)
				}
				p.restartDurableState(t)
				round := 2
				if transition == engine.DurableTransitionReviewRequest &&
					side == engine.DurableTransitionAfter {
					// The interrupted request is abandoned for the next round,
					// as on the first cycle.
					round = 3
				}
				p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
				p.scriptCleanReview(3, advanced, p.replay.HeadSHA)
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				p.reconcileUntilReady(t, 3)
				ready := p.assertReentered(t, superseded, round, advanced, p.replay.HeadSHA)
				if got := p.baseWatchBase(t, ready.ID); got != advanced {
					t.Fatalf("watch base = %s, want cycle base %s", got, advanced)
				}
				p.restartDurableState(t)
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				for range 2 {
					if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
						t.Fatalf("converged replay = %#v, %v", result, err)
					}
				}
				if got := p.currentReadyItem(t); got.ID != ready.ID || got.ItemVersion != ready.ItemVersion {
					t.Fatalf("restart rewrote the ready item: %#v", got)
				}
				for id, count := range reviewCalls.requestIDs {
					if count != 1 {
						t.Fatalf("restart requested review %s %d times, want 1", id, count)
					}
				}
				wantRuns := 2
				if transition == engine.DurableTransitionVerificationEvidence &&
					side == engine.DurableTransitionBefore {
					wantRuns = 3
				}
				if p.room.runs != wantRuns {
					t.Fatalf("verification runs = %d, want %d", p.room.runs, wantRuns)
				}
			})
		}
	}
}

// TestReadinessReentryDiscardsCycleWhenHeadMoves: a head pushed while a cycle
// runs is not an external conflict. The cycle finishes on its sealed head,
// records its ready item already superseded, and the run re-enters for the
// pushed head.
func TestReadinessReentryDiscardsCycleWhenHeadMoves(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	second := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: second,
	})
	if !started {
		t.Fatal("a head change started no re-entry")
	}
	p.scriptCleanReview(2, p.baseSHA, second)
	var third string
	p.transport.onHeadFetch(func(call int) {
		// The second fetch of the cycle is its check of the live head just
		// before readiness.
		if call == 2 {
			third = p.pushToPullRequest(t, "LATER.md", "pushed during the cycle\n")
			p.scriptCleanReview(3, p.baseSHA, third)
		}
	})
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if third == "" {
		t.Fatal("the cycle never checked the live head before readiness")
	}
	var discarded domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if len(chain) != 2 || chain[1].Reentry == nil || chain[1].Reentry.HeadSHA != third ||
			chain[1].PredecessorItemID != chain[0].ReadyItemID() {
			t.Fatalf("successor chain = %#v", chain)
		}
		if chain[0].PredecessorItemID != superseded.ID {
			t.Fatalf("first cycle predecessor = %s, want %s", chain[0].PredecessorItemID, superseded.ID)
		}
		if discarded, err = tx.GetAttentionItem(p.ctx, chain[0].ReadyItemID()); err != nil {
			return err
		}
		if hold, held, err := tx.GetRunHold(p.ctx, p.runID); err != nil || held {
			t.Errorf("a moved head left run hold %#v, %v", hold, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fact := discarded.ReadinessInvalidation
	// Version one is the superseded record: the item has no open version.
	if discarded.Status != domain.StatusSuperseded || discarded.ItemVersion != 1 || fact == nil ||
		fact.Reason != domain.ReadinessInvalidationHeadChanged ||
		fact.Bound != second || fact.Observed != third {
		t.Fatalf("discarded ready item = %#v", discarded)
	}
	p.transport.onHeadFetch(nil)
	p.reconcileUntilReady(t, 2)
	p.assertReentered(t, discarded, 3, p.baseSHA, third)
	if p.room.runs != 3 {
		t.Fatalf("verification runs = %d, want 3", p.room.runs)
	}
}

// TestReadinessReentryStopsOnHeadTheBranchLacks: a head the pull request's
// branch does not contain is refused before anything is verified, and the
// stop names the commit.
func TestReadinessReentryStopsOnHeadTheBranchLacks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	absent := strings.Repeat("a", 40)
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: absent,
	}); !started {
		t.Fatal("a head change started no re-entry")
	}
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("missing head result = %#v, %v", result, err)
	}
	stop := p.reentryStopItem(t)
	if !strings.Contains(stop.Reason, absent) || !stop.Offers(domain.ActionStop) ||
		!stop.Offers(domain.ActionOpenPR) {
		t.Fatalf("stop item = %#v", stop)
	}
	if p.room.runs != 1 {
		t.Fatalf("a refused head was verified: %d runs", p.room.runs)
	}
}

// reentryStopItem returns the open stop item of the run's current cycle and
// requires that it carries the pull request and left no hold to retry.
func (p *productionPublicationHarness) reentryStopItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	var stop domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if stop, err = tx.GetAttentionItem(p.ctx, successor.BlockedItemID()); err != nil {
			return err
		}
		held, err := tx.GetHeldItemPRBinding(p.ctx, stop.ID)
		if err != nil {
			return err
		}
		if held.PRNumber != p.forge.pullRequests()[0].Number {
			t.Errorf("stop item pull request = %d", held.PRNumber)
		}
		if _, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a stopped cycle wrote a ready item: %v", err)
		}
		if hold, held, err := tx.GetRunHold(p.ctx, p.runID); err != nil || held {
			t.Errorf("a stopped cycle left run hold %#v, %v", hold, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stop.Status != domain.StatusOpen || stop.Type != domain.AttentionPublishBlocked {
		t.Fatalf("stop item = %#v", stop)
	}
	return stop
}

// TestReadinessReentryStopsOnFailedVerification: a cycle whose verification
// does not pass asks for no review and stops with the evidence attached.
func TestReadinessReentryStopsOnFailedVerification(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	}); !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.room.fail = true
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("failed verification result = %#v, %v", result, err)
	}
	stop := p.reentryStopItem(t)
	if len(stop.EvidenceSnapshot) == 0 || !strings.Contains(stop.Reason, advanced) {
		t.Fatalf("stop item = %#v", stop)
	}
	var review domain.ReviewRecord
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		review, err = tx.LatestReviewRecord(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if review.Round != 1 {
		t.Fatalf("a failed verification still reached review round %d", review.Round)
	}
}

// TestReadinessReentryStopsOnTreeVerificationRefuses: a pushed head whose tree
// the verifier refuses (here a recipe entrypoint turned into a symlink) stops
// its own cycle on an item a person can read. It must not surface as a lane
// error, which would stop publication for every run on each pass.
func TestReadinessReentryStopsOnTreeVerificationRefuses(t *testing.T) {
	t.Parallel()
	h := newPublicationHarnessWithRecipe(t,
		[]byte(`{"commands":[["scripts/verify.sh"]],"capture":"none"}`))
	p := newProductionPublicationHarnessFromBase(t, h, "", nil, nil, nil)
	p.publishReady(t)
	head := p.pushCommitToPullRequest(t, func(dir string) {
		if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../README.md", filepath.Join(dir, "scripts", "verify.sh")); err != nil {
			t.Fatal(err)
		}
	})
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: head,
	}); !started {
		t.Fatal("a head change started no re-entry")
	}
	runs := p.room.runs
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("unverifiable tree result = %#v, %v", result, err)
	}
	stop := p.reentryStopItem(t)
	if !strings.Contains(stop.Reason, head) || !strings.Contains(stop.Reason, "symbolic link") ||
		!strings.Contains(stop.Reason, "scripts/verify.sh") {
		t.Fatalf("stop item = %#v", stop)
	}
	if p.room.runs != runs {
		t.Fatalf("a refused tree ran %d verification commands", p.room.runs-runs)
	}
	// The task is retired: later passes neither fail nor repeat the stop.
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 0 {
		t.Fatalf("pass after the stop = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil || len(pending) != 0 {
			t.Errorf("a stopped cycle left %d pending tasks, %v", len(pending), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessReentryStopWithdrawsItsOwnReadyItem: a process lost right after
// the cycle's ready item is written, then a force-push that drops the sealed
// head before the next pass. The pass stops, and the item it finds open must
// not go on claiming readiness beside the stop.
func TestReadinessReentryStopWithdrawsItsOwnReadyItem(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	}); !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	injected := false
	p.workflow = p.newEngine(t, productionCrashSeams{
		transitionHook: func(got engine.DurableTransition, side engine.DurableTransitionSide) error {
			if !injected && got == engine.DurableTransitionReadyItem && side == engine.DurableTransitionAfter {
				injected = true
				return errors.New("injected process loss")
			}
			return nil
		},
	}, true)
	if _, err := p.reconcileLanes(); err == nil || !injected {
		t.Fatalf("the ready item write was not interrupted: %v", err)
	}
	if ready := p.currentReadyItem(t); ready.Status != domain.StatusOpen {
		t.Fatalf("interrupted cycle's ready item = %#v", ready)
	}
	// Someone replaces the branch with history that lacks the sealed head.
	replacement := t.TempDir()
	runGit(t, replacement, "init", "-q")
	if err := os.WriteFile(filepath.Join(replacement, "REWRITTEN.md"), []byte("rewritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, replacement, "add", "-A")
	runGit(t, replacement, "commit", "-q", "-m", "rewrite the branch")
	p.transport.pushBranchHead(replacement, p.forge.pullRequests()[0].HeadRef,
		runGit(t, replacement, "rev-parse", "HEAD"))

	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("pass after the force-push = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		ready, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID())
		if err != nil {
			return err
		}
		if ready.Status != domain.StatusSuperseded {
			t.Errorf("stopped cycle's ready item = status %s, want superseded", ready.Status)
		}
		stop, err := tx.GetAttentionItem(p.ctx, successor.BlockedItemID())
		if err != nil {
			return err
		}
		if stop.Status != domain.StatusOpen || !strings.Contains(stop.Reason, p.replay.HeadSHA) {
			t.Errorf("stop item = %#v", stop)
		}
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil || len(pending) != 0 {
			t.Errorf("a stopped cycle left %d pending tasks, %v", len(pending), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessReentryFindingsRaiseReviewItem: findings on a re-entered cycle
// go to a person. No adjudicator or remediation runs, since Freeside did not
// produce the commits under review.
func TestReadinessReentryFindingsRaiseReviewItem(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	}); !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: advanced, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("re-entry findings")),
			Findings: []domain.Finding{{
				ID: "reentry-finding-1", RunID: p.runID, Source: "codex_local", Severity: "P1",
				Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
				Message:  "unsafe after the merge", RawText: "unsafe after the merge",
				CreatedAt: p.now,
			}},
		},
	})
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("findings result = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionReviewDispute || item.Status != domain.StatusOpen ||
			item.PRHeadSHA != p.replay.HeadSHA {
			t.Errorf("review item = %#v", item)
		}
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if _, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("findings still wrote a ready item: %v", err)
		}
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Errorf("findings left %d publication tasks pending", len(pending))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("findings replay = %#v, %v", result, err)
	}
}

// TestReadinessReentryHoldCarriesThePullRequest: a repairable refusal holds
// the cycle instead of stopping it, the hold item links the pull request, and
// the cycle resumes once the cause is repaired.
func TestReadinessReentryHoldCarriesThePullRequest(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	})
	if !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true,
		map[domain.Digest]bool{productionDigest([]byte("another recipe")): true})
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("held result = %#v, %v", result, err)
	}
	var holdItem domain.ItemID
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		holdItem = successor.BlockedItemID()
		held, err := tx.GetHeldItemPRBinding(p.ctx, holdItem)
		if err != nil {
			return err
		}
		if held.PRNumber != p.forge.pullRequests()[0].Number {
			t.Errorf("hold item pull request = %d", held.PRNumber)
		}
		hold, present, err := tx.GetRunHold(p.ctx, p.runID)
		if err != nil || !present || hold.Reason != domain.HoldRecipeRevoked {
			t.Errorf("run hold = %#v, %t, %v", hold, present, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if p.room.runs != 1 {
		t.Fatalf("a held cycle ran verification: %d runs", p.room.runs)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reconcileUntilReady(t, 2)
	p.assertReentered(t, superseded, 2, advanced, p.replay.HeadSHA)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(p.ctx, holdItem)
		if err != nil {
			return err
		}
		if item.Status == domain.StatusOpen {
			t.Errorf("hold item stayed open beside the ready item: %#v", item)
		}
		if hold, present, err := tx.GetRunHold(p.ctx, p.runID); err != nil || present {
			t.Errorf("run hold after readiness = %#v, %v", hold, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessReentryFindingsEndAnEarlierHold: a cycle that was held, then
// repaired, and then ends on review findings retires its task, so no retry
// remains for the hold to describe. The hold item and the run hold end with
// it instead of staying open beside the review item.
func TestReadinessReentryFindingsEndAnEarlierHold(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	}); !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.workflow = p.newEngineWithApprovedRecipes(t, productionCrashSeams{}, true,
		map[domain.Digest]bool{productionDigest([]byte("another recipe")): true})
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("held result = %#v, %v", result, err)
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: advanced, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("re-entry findings")),
			Findings: []domain.Finding{{
				ID: "reentry-finding-1", RunID: p.runID, Source: "codex_local", Severity: "P1",
				Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
				Message:  "unsafe after the merge", RawText: "unsafe after the merge",
				CreatedAt: p.now,
			}},
		},
	})
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
		if err != nil {
			return err
		}
		if review.Type != domain.AttentionReviewDispute || review.Status != domain.StatusOpen {
			t.Errorf("review item = %#v", review)
		}
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		hold, err := tx.GetAttentionItem(p.ctx, successor.BlockedItemID())
		if err != nil {
			return err
		}
		if hold.Status == domain.StatusOpen {
			t.Errorf("hold item stayed open beside the review item: %#v", hold)
		}
		if runHold, present, err := tx.GetRunHold(p.ctx, p.runID); err != nil || present {
			t.Errorf("run hold after the cycle ended = %#v, %v", runHold, err)
		}
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Errorf("findings left %d publication tasks pending", len(pending))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessReentryStartsOnlyFromTheCurrentInvalidation covers what starts
// no cycle: an invalidation the cycle cannot answer, a head change that does
// not start from the bound head, and the second of two writers that observed
// one invalidation.
func TestReadinessReentryStartsOnlyFromTheCurrentInvalidation(t *testing.T) {
	t.Parallel()
	t.Run("retargeted", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarness(t, "")
		p.publishReady(t)
		if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationRetargeted, Bound: "main", Observed: "release",
		}); started {
			t.Fatal("a retargeted pull request re-entered")
		}
		p.assertNoPublicationTask(t)
	})
	t.Run("head change from another head", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarness(t, "")
		p.publishReady(t)
		if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationHeadChanged,
			Bound:  strings.Repeat("b", 40), Observed: strings.Repeat("c", 40),
		}); started {
			t.Fatal("a head change bound to another head re-entered")
		}
		p.assertNoPublicationTask(t)
	})
	t.Run("second writer", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarness(t, "")
		p.publishReady(t)
		advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
		superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
		})
		if !started {
			t.Fatal("a base advance started no re-entry")
		}
		if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
			again, err := engine.StartReadinessReentry(p.ctx, tx, superseded)
			if again {
				t.Error("one invalidation started two cycles")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
			if err != nil {
				return err
			}
			pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
			if err != nil {
				return err
			}
			if len(chain) != 1 || len(pending) != 1 {
				t.Errorf("chain = %d, pending tasks = %d, want one each", len(chain), len(pending))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func (p *productionPublicationHarness) assertNoPublicationTask(t *testing.T) {
	t.Helper()
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if len(pending) != 0 || len(chain) != 0 {
			t.Errorf("skipped invalidation left %d tasks and %d successors", len(pending), len(chain))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessReentryRefusesMergeThatDiffersFromTheVerifiedOne: the stored
// verification is evidence for one merge commit. A rebuild that yields
// another commit for the same pair is refused loudly, never reviewed or made
// ready on the earlier evidence.
func TestReadinessReentryRefusesMergeThatDiffersFromTheVerifiedOne(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	}); !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error { return errors.New("injected process loss") },
	}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("the crash seam after verification did not interrupt the cycle")
	}
	p.restartDurableState(t)
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	reviews := p.recordReviews(t)
	p.transport.rebuildMergesAs("another merge of the same pair")
	if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("differing merge: err = %v, want a parent-key mismatch", err)
	}
	if len(reviews.requests) != 0 || p.room.runs != 2 {
		t.Fatalf("a differing merge reached review (%d requests) or re-verified (%d runs)",
			len(reviews.requests), p.room.runs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if _, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a differing merge wrote a ready item: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessReentryReadsItsVerdictFromTheVerificationReport: the cycle
// stores no candidate authorization, so the checkpoint row is the only place
// its outcome is written down. A row that claims a pass its own report does
// not record is a contradiction, never a clean verification.
func TestReadinessReentryReadsItsVerdictFromTheVerificationReport(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	}); !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.room.fail = true
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error { return errors.New("injected process loss") },
	}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("the crash seam after verification did not interrupt the cycle")
	}
	p.restartDurableState(t)

	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	}()
	const kind = "production_reentry_verification_checkpoint"
	var payload []byte
	if err := raw.QueryRowContext(p.ctx,
		"SELECT payload FROM inbox WHERE kind=?", kind).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	failed, passed := []byte(`"outcome":"failed"`), []byte(`"outcome":"passed"`)
	if bytes.Count(payload, failed) != 1 {
		t.Fatalf("checkpoint does not record one failed outcome: %s", payload)
	}
	if _, err := raw.ExecContext(p.ctx, "UPDATE inbox SET payload=? WHERE kind=?",
		bytes.Replace(payload, failed, passed, 1), kind); err != nil {
		t.Fatal(err)
	}

	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	reviews := p.recordReviews(t)
	if _, err := p.reconcileLanes(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("altered verdict: err = %v, want a parent-key mismatch", err)
	}
	if len(reviews.requests) != 0 {
		t.Fatalf("an altered verdict reached review (%d requests)", len(reviews.requests))
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if _, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an altered verdict wrote a ready item: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUnreadableReentryTaskDoesNotEndTheEngineLoop: a re-entry row this
// daemon cannot decode is attributed to its run by key and quarantined, like
// any other publication task row (#424), so the lane keeps serving other
// runs.
func TestUnreadableReentryTaskDoesNotEndTheEngineLoop(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	stranded := domain.RunID("run-unreadable-reentry")
	seedFutureVersionProductionRun(t, p, stranded, "freeside.production-invocation/v2")
	key := domain.PublicationSuccessor{
		Version: domain.PublicationReentryVersion, RunID: stranded,
		PredecessorItemID:       domain.ItemID("production-ready-" + string(stranded)),
		PriorReviewInvocationID: engine.ProductionReviewInvocationID(stranded, 1),
		ReviewRound:             2,
		Origin:                  domain.PublicationSuccessorReadinessInvalidation,
		Reentry: &domain.PublicationSuccessorReentry{
			Reason:  domain.ReadinessInvalidationBaseAdvanced,
			BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40),
		},
	}.TaskKey()
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(
			p.ctx, key, engine.KindProductionPublicationRequested,
			[]byte(`{"version":9,"run_id":"run-unreadable-reentry"}`),
		)
		return err
	}); err != nil {
		t.Fatalf("seed unreadable re-entry task: %v", err)
	}
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("reconcile beside an unreadable re-entry task: %v", err)
	}
	p.assertReady(t)
	var item domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(
			p.ctx, domain.ItemID("production-task-quarantined-1-"+string(stranded)))
		return err
	}); err != nil {
		t.Fatalf("read task quarantine item: %v", err)
	}
	if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen {
		t.Fatalf("task quarantine item = %#v", item)
	}
}
