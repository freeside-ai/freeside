package integration_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// landRebase rewrites the published head the way a lander's rebase does,
// keeping its tree and parent under a new commit, and moves the pull request
// to it. It returns the landed head and its tree.
func (p *productionPublicationHarness) landRebase(t *testing.T) (head, tree string) {
	t.Helper()
	pull := p.forge.PullRequests()[0]
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "fetch", "-q", headRepo(t, p.transport), "refs/heads/"+pull.HeadRef)
	tree = runGit(t, dir, "rev-parse", "FETCH_HEAD^{tree}")
	head = runGit(t, dir, "commit-tree", tree, "-p", "FETCH_HEAD^", "-m", "rebased by the lander")
	if err := p.transport.LandPullHead(dir, pull.HeadRef, pull.Number, head); err != nil {
		t.Fatal(err)
	}
	return head, tree
}

// startRebaseReentry lands a rebase of the published head and starts the
// head_changed cycle it causes.
func (p *productionPublicationHarness) startRebaseReentry(t *testing.T) (superseded domain.AttentionItem, head, tree string) {
	t.Helper()
	p.publishReady(t)
	head, tree = p.landRebase(t)
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: head,
	})
	if !started {
		t.Fatal("a head change started no re-entry")
	}
	return superseded, head, tree
}

// rebaseState reads what a rebase cycle leaves for completion: the proof row
// for head, the head completion compares, and the latest review.
func (p *productionPublicationHarness) rebaseState(
	t *testing.T, head string,
) (proof domain.WorkUnitRebaseProof, effectiveHead string, review domain.ReviewRecord, proofErr error) {
	t.Helper()
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		declaration, err := tx.GetWorkUnitDeclarationByRun(p.ctx, p.runID)
		if err != nil {
			return err
		}
		proof, proofErr = tx.GetWorkUnitRebaseProof(p.ctx, declaration.ID, head)
		effective, err := tx.EffectiveWorkUnitPRBinding(p.ctx, declaration.ID)
		if err != nil {
			return err
		}
		effectiveHead = effective.HeadSHA
		review, err = tx.LatestReviewRecord(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return proof, effectiveHead, review, proofErr
}

// assertQuietEnd requires that the cycle raised nothing and retired its task.
func (p *productionPublicationHarness) assertQuietEnd(t *testing.T) {
	t.Helper()
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if _, err := tx.GetAttentionItem(p.ctx, successor.BlockedItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a quiet end raised a hold: %v", err)
		}
		if _, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a quiet end wrote a ready item: %v", err)
		}
		if hold, held, err := tx.GetRunHold(p.ctx, p.runID); err != nil || held {
			t.Errorf("a quiet end left run hold %#v, %v", hold, err)
		}
		entry, err := tx.GetOutbox(p.ctx, successor.TaskKey())
		if err != nil {
			return err
		}
		if !entry.Dispatched() {
			t.Error("a quiet end left its task pending")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadinessReentryEndsQuietlyOnAProvenRebase: a lander's rebase of the
// reviewed head onto the reviewed base needs no new review. The cycle
// records the proof, verifies and reviews nothing, raises nothing, and
// moves the head completion compares to the landed one.
func TestReadinessReentryEndsQuietlyOnAProvenRebase(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 443)
	_, head, tree := p.startRebaseReentry(t)
	p.transport.ScriptRebaseProof(publish.RebaseProof{Proven: true, TreeSHA: tree})

	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("proven rebase result = %#v, %v", result, err)
	}
	p.assertQuietEnd(t)
	proof, effectiveHead, review, proofErr := p.rebaseState(t, head)
	if proofErr != nil || !proof.Proven || proof.ReviewedHeadSHA != p.replay.HeadSHA ||
		proof.ReviewedBaseSHA != p.baseSHA || proof.ReviewedTreeSHA != tree {
		t.Fatalf("proof = %#v, %v", proof, proofErr)
	}
	if effectiveHead != head {
		t.Errorf("completion compares head %s, want the proven %s", effectiveHead, head)
	}
	if review.Round != 1 || p.room.runs != 1 {
		t.Errorf("a proven rebase was reviewed (round %d) or verified (%d runs)", review.Round, p.room.runs)
	}

	// A later pass has nothing left to do and proves nothing again.
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("second pass = %#v, %v", result, err)
	}
	if _, proofs := p.transport.ProofCounts(); proofs != 1 {
		t.Errorf("proofs = %d, want 1", proofs)
	}
}

// TestReadinessReentryReviewsARefusedRebase: a head the proof refuses is
// reviewed as any pushed head is, and the refusal stays on record.
func TestReadinessReentryReviewsARefusedRebase(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 443)
	superseded, head, tree := p.startRebaseReentry(t)
	p.transport.ScriptRebaseProof(publish.RebaseProof{
		TreeSHA: tree, Detail: "range-diff does not pair the reviewed commits as identical",
	})
	p.scriptCleanReview(2, p.baseSHA, head)

	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("refused rebase result = %#v, %v", result, err)
	}
	p.assertReentered(t, superseded, 2, p.baseSHA, head)
	proof, _, _, proofErr := p.rebaseState(t, head)
	if proofErr != nil || proof.Proven || proof.Detail == "" {
		t.Fatalf("refusal = %#v, %v", proof, proofErr)
	}
}

// TestReadinessReentryEndsQuietlyWhenThePullRequestMerged: a lander that
// merges and deletes the branch leaves a head the cycle cannot fetch by
// branch. Whether the merge is on record before the cycle starts or arrives
// while it runs, the cycle raises no hold; completion or foreclosure is the
// reconciler's answer.
func TestReadinessReentryEndsQuietlyWhenThePullRequestMerged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		midway bool
	}{
		{name: "merged before the cycle"},
		{name: "merged while the cycle runs", midway: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarnessWithBoundIssue(t, "", 443)
			_, head, _ := p.startRebaseReentry(t)
			pull := p.forge.PullRequests()[0]
			if err := p.transport.DeleteBranch(pull.HeadRef); err != nil {
				t.Fatal(err)
			}
			recordMerge := func() {
				var predecessor domain.ReadyItemPRBinding
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
					if err != nil {
						return err
					}
					predecessor, err = tx.GetReadyItemPRBinding(p.ctx, successor.PredecessorItemID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
					_, err := tx.AppendPullMergeFact(p.ctx, domain.PullMergeFact{
						Repo: predecessor.Repo, RepositoryID: predecessor.RepositoryID,
						PRNumber: predecessor.PRNumber, State: domain.PullRequestClosed, Merged: true,
						MergeCommitSHA: "1234567890abcdef1234567890abcdef12345678",
						BaseRef:        predecessor.BaseRef, HeadSHA: head, ObservedAt: p.now.UTC(),
					})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.midway {
				// The proof is unscripted, so it fails as unprovable and the
				// cycle goes on to fetch the deleted branch; the merge lands
				// just before that fetch.
				p.transport.OnHeadFetch(func(int) { recordMerge() })
			} else {
				recordMerge()
			}

			result, err := p.reconcileLanes()
			if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
				t.Fatalf("merged result = %#v, %v", result, err)
			}
			p.assertQuietEnd(t)
			if _, _, _, proofErr := p.rebaseState(t, head); !errors.Is(proofErr, store.ErrNotFound) {
				t.Errorf("an unprovable head recorded a verdict: %v", proofErr)
			}
		})
	}
}
