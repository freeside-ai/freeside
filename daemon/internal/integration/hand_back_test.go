package integration_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// handBack does what the lander and the reconciler leave behind when the
// lander hands a pull request back on head (issue #3160): the pull request
// recorded open on that head, and reviewer's conversation comment on it. It
// then runs the trigger as the reconciler does, with base as the merge base
// the forge reports, and reports whether a cycle started.
func (p *productionPublicationHarness) handBack(
	t *testing.T, published domain.AttentionItem, comment int, reviewer domain.ExternalReviewer,
	head, base, text string,
) (domain.Finding, bool) {
	t.Helper()
	finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: p.runID, Forge: reviewer.Forge,
		ReviewerAccountID: reviewer.AccountID, ReviewerLogin: reviewer.Login,
		ThreadID: fmt.Sprintf("issue_comment/%d", comment), HeadSHA: head,
		Severity: "P2", Message: text, RawText: text,
		CreatedAt: p.now.UTC().Add(time.Duration(comment) * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		binding, err := tx.GetReadyItemPRBinding(p.ctx, published.ID)
		if err != nil {
			return err
		}
		if _, err := tx.AppendPullMergeFact(p.ctx, domain.PullMergeFact{
			Repo: binding.Repo, RepositoryID: binding.RepositoryID, PRNumber: binding.PRNumber,
			State: domain.PullRequestOpen, BaseRef: binding.BaseRef, HeadSHA: head, ObservedAt: p.now,
		}); err != nil {
			return err
		}
		return tx.PutExternalFinding(p.ctx, finding)
	}); err != nil {
		t.Fatal(err)
	}
	var due bool
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		due, err = engine.HandBackReentryDue(p.ctx, tx, p.runID, head)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !due {
		return finding, false
	}
	var started bool
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		var err error
		_, started, err = engine.StartHandBackReentry(p.ctx, tx, p.runID, head, base)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("the probe reported a hand-back due and the writer started none")
	}
	return finding, true
}

// TestHandBackCycleHandsOffTheAdoptedHead drives issue #3160 end to end. The
// lander pushes a head Freeside did not write and hands the pull request back
// with a comment, and the target moves on. The cycle verifies and reviews that
// head against its merge base, adjudication answers the comment, and before
// the run is ready again the cycle hands the head off onto the moved tip: the
// merge is verified under the cycle's own handoff key, the ready item names
// it, and nothing is pushed.
func TestHandBackCycleHandsOffTheAdoptedHead(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	first := p.publishReady(t)
	maintainer := externalMaintainer()
	p.listExternalReviewers(t, maintainer)
	head := p.pushToPullRequest(t, "REWORK.md", "the lander's rework\n")
	tip := p.advanceBase(t, "TIP.md", "a change that landed meanwhile\n")
	p.audit.AuditedCommitSHA = tip
	pushes := p.transport.PushCount()

	comment, started := p.handBack(t, first, 1, maintainer, head, p.baseSHA, "the rebase broke the build")
	if !started {
		t.Fatal("a hand-back comment on the adopted head started no cycle")
	}
	if _, again := p.handBack(t, first, 1, maintainer, head, p.baseSHA, "the rebase broke the build"); again {
		t.Fatal("one comment started a second hand-back cycle")
	}
	reviews := p.recordReviews(t)
	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(comment.ID))
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	if len(reviews.requests) != 1 || reviews.requests[0].BaseSHA != p.baseSHA ||
		reviews.requests[0].HeadSHA != head {
		t.Fatalf("review requests = %#v, want one of %s at its merge base %s", reviews.requests, head, p.baseSHA)
	}
	if p.transport.MergeCount() != 1 || p.transport.PushCount() != pushes {
		t.Fatalf("merges = %d, pushes %d (was %d); want the handoff merge and no push",
			p.transport.MergeCount(), p.transport.PushCount(), pushes)
	}

	var (
		successor domain.PublicationSuccessor
		check     domain.HandoffCheck
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil || current == nil {
			return fmt.Errorf("current successor %v: %w", current, err)
		}
		successor = *current
		check, err = tx.GetHandoffCheck(p.ctx, p.runID, successor.InPlaceHandoffID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !successor.HandsBack() || successor.Reentry.HeadSHA != head || successor.Reentry.BaseSHA != p.baseSHA {
		t.Fatalf("authority = %#v (re-entry %#v), want a hand_back cycle on %s", successor, successor.Reentry, head)
	}
	if !check.Proved() || check.TipSHA != tip || check.HeadSHA != head || check.EvidenceBaseSHA != p.baseSHA ||
		check.Merge == nil {
		t.Fatalf("in-place handoff check = %#v, want a verified merge of %s into %s", check, head, tip)
	}
	ready := p.currentReadyItem(t)
	if ready.ID != successor.ReadyItemID() || ready.Status != domain.StatusOpen || ready.PRHeadSHA != head {
		t.Fatalf("ready item = %#v", ready)
	}
	detail := ready.ReadinessDetail
	if detail == nil || detail.HandoffTip != tip || detail.HandoffMerge == nil ||
		detail.HandoffMerge.HeadSHA != head || detail.HandoffMerge.MergeSHA != check.Merge.MergeSHA {
		t.Fatalf("ready detail = %#v, want the handoff of %s onto %s", detail, head, tip)
	}
}

// TestHandBackCycleContinuesARebaseThatEndedQuietly drives the common
// hand-back (issue #3160): the lander's rebase ends the head_changed cycle it
// causes quietly, with no ready item, then CI fails and the lander hands the
// rebased head back. The hand-back's predecessor is that quiet cycle's item,
// which was never written, so the cycle continues the pull request the last
// published item bound, and re-earns readiness on the rebased head.
func TestHandBackCycleContinuesARebaseThatEndedQuietly(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 443)
	driver := p.configureAdjudicator(t)
	first, head, tree := p.startRebaseReentry(t)
	p.transport.ScriptRebaseProof(publish.RebaseProof{Proven: true, TreeSHA: tree})
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("proven rebase result = %#v, %v", result, err)
	}
	p.assertQuietEnd(t)
	maintainer := externalMaintainer()
	p.listExternalReviewers(t, maintainer)

	comment, started := p.handBack(t, first, 1, maintainer, head, p.baseSHA, "CI failed after the rebase")
	if !started {
		t.Fatal("a hand-back after a quiet rebase cycle started no cycle")
	}
	p.scriptCleanReview(2, p.baseSHA, head)
	scriptAdjudication(t, driver, declineVerdict(comment.ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 || result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}

	var successor domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil || current == nil {
			return fmt.Errorf("current successor %v: %w", current, err)
		}
		successor = *current
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil || len(chain) != 2 {
			return fmt.Errorf("chain of %d: %w", len(chain), err)
		}
		if successor.PredecessorItemID != chain[0].ReadyItemID() {
			return fmt.Errorf("predecessor %s, want the quiet cycle's item %s",
				successor.PredecessorItemID, chain[0].ReadyItemID())
		}
		if _, err := tx.GetReadyItemPRBinding(p.ctx, successor.PredecessorItemID); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("the quiet cycle's item has a binding: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !successor.HandsBack() || successor.Reentry.HeadSHA != head {
		t.Fatalf("authority = %#v, want a hand_back cycle on %s", successor, head)
	}
	ready := p.currentReadyItem(t)
	if ready.ID != successor.ReadyItemID() || ready.Status != domain.StatusOpen || ready.PRHeadSHA != head {
		t.Fatalf("ready item = %#v", ready)
	}
	if got := p.externalDispositions(t); len(got) != 1 || got[0].FindingID != comment.ID ||
		got[0].Disposition != domain.ReviewDispositionDeclined {
		t.Fatalf("dispositions = %#v, want the comment declined", got)
	}
}
