package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reentryRebaseSubject is what a head_changed cycle may prove a rebase
// against: the reviewed pair and the work unit the proof belongs to.
type reentryRebaseSubject struct {
	unit         domain.WorkUnitPRBinding
	reviewedBase string
	reviewedHead string
}

// proveReentryRebase is the first step of a head_changed cycle (issue #2129).
// A lander that requires up-to-date branches rebases the reviewed head and
// force-pushes before it merges, so the head this cycle was started for is
// often the reviewed candidate on the reviewed base, which needs no new
// review. When the transport proves that, the cycle records the proof and
// ends quietly: no review, no ready item, no hold. Completion then accepts a
// merge of the proven head through the store's effective binding, and
// readiness stays bound to the reviewed head.
//
// done is false when the cycle continues to review the head as before: the
// head_changed reason does not apply, the run declares no work unit, the
// reviewed pair is not the latest clean review's, the transport has no proof
// seam or could not fetch either head, or the proof was refused. A refusal is
// recorded so a merge of that head forecloses instead of waiting on a proof;
// a failed fetch records nothing, because it is not a verdict.
func (w *productionPublicationWorkflow) proveReentryRebase(
	ctx context.Context, task productionPublicationTask, checkout PublicationCheckout,
	predecessor domain.ReadyItemPRBinding,
) (done bool, err error) {
	if task.Successor.Reentry.Reason != domain.ReadinessInvalidationHeadChanged {
		return false, nil
	}
	transport, ok := w.transport.(RebaseProofTransport)
	if !ok {
		return false, nil
	}
	subject, found, err := w.reentryRebaseSubject(ctx, task, predecessor)
	if err != nil || !found {
		return false, err
	}
	var recorded domain.WorkUnitRebaseProof
	readErr := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recorded, err = tx.GetWorkUnitRebaseProof(ctx, subject.unit.UnitID, task.HeadSHA)
		return err
	})
	switch {
	case readErr == nil:
		// A pass that recorded the verdict and lost its process before
		// retiring the task finishes from the record, never from a second
		// proof that could disagree with it.
		if recorded.ReviewedHeadSHA != subject.reviewedHead || recorded.ReviewedBaseSHA != subject.reviewedBase {
			return false, nil
		}
		if !recorded.Proven {
			return false, nil
		}
		return true, w.endReentryCycleQuietly(ctx, task, reentryRebaseProvenReason(task, subject), nil)
	case !errors.Is(readErr, store.ErrNotFound):
		return false, readErr
	}
	if err := transport.FetchPullHead(ctx, checkout, predecessor.PRNumber, task.HeadSHA, subject.reviewedHead); err != nil {
		if unprovable(err) {
			return false, nil
		}
		return false, err
	}
	verdict, err := transport.ProveRebase(ctx, checkout, subject.reviewedBase, subject.reviewedHead, task.HeadSHA)
	if err != nil {
		if unprovable(err) {
			return false, nil
		}
		return false, err
	}
	proof := domain.WorkUnitRebaseProof{
		UnitID: subject.unit.UnitID, RepositoryID: subject.unit.RepositoryID, PRNumber: subject.unit.PRNumber,
		ReviewedBaseSHA: subject.reviewedBase, ReviewedHeadSHA: subject.reviewedHead,
		ProvenHeadSHA: task.HeadSHA, ReviewedTreeSHA: verdict.TreeSHA,
		Proven: verdict.Proven, Detail: verdict.Detail, RecordedAt: w.now().UTC(),
	}
	record := func(tx *store.InternalTx) error { return tx.RecordWorkUnitRebaseProof(ctx, proof) }
	if !proof.Proven {
		return false, w.store.WriteInternal(ctx, record)
	}
	return true, w.endReentryCycleQuietly(ctx, task, reentryRebaseProvenReason(task, subject), record)
}

// reentryRebaseSubject loads the reviewed pair a proof compares against and
// the unit it is recorded for. found is false when the run declares no work
// unit or the reviewed pair is not one the store's effective binding would
// honor a proof for: the latest review must be clean and cover exactly the
// predecessor's published head on this cycle's base, on the unit's pull
// request. predecessor is the cycle's, which its loader already proved.
func (w *productionPublicationWorkflow) reentryRebaseSubject(
	ctx context.Context, task productionPublicationTask, predecessor domain.ReadyItemPRBinding,
) (subject reentryRebaseSubject, found bool, err error) {
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, task.RunID)
		if err != nil {
			return err
		}
		unit, err := tx.GetWorkUnitPRBinding(ctx, declaration.ID)
		if err != nil {
			return err
		}
		review, err := tx.LatestReviewRecord(ctx, task.RunID)
		if err != nil {
			return err
		}
		found = review.Outcome == domain.ReviewClean &&
			predecessor.RunID == task.RunID &&
			unit.RepositoryID == predecessor.RepositoryID && unit.PRNumber == predecessor.PRNumber &&
			review.HeadSHA == predecessor.HeadSHA && review.BaseSHA == task.Successor.Reentry.BaseSHA &&
			task.HeadSHA != review.HeadSHA
		subject = reentryRebaseSubject{unit: unit, reviewedBase: review.BaseSHA, reviewedHead: review.HeadSHA}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return reentryRebaseSubject{}, false, nil
	}
	return subject, found, err
}

// unprovable reports a transport failure that leaves the head unproven
// rather than failing the pass: the cycle then reviews the head as before,
// and that path fetches and classifies the same remote itself.
func unprovable(err error) bool {
	return errors.Is(err, publish.ErrGitTransport) || errors.Is(err, publish.ErrReviewedHeadUnavailable) ||
		errors.Is(err, publish.ErrRemoteMissingHead) || errors.Is(err, publish.ErrRemoteMissingBase)
}

func reentryRebaseProvenReason(task productionPublicationTask, subject reentryRebaseSubject) string {
	return fmt.Sprintf(
		"Superseded because pull request head %s is the reviewed head %s rebased onto the reviewed base %s, so it needs no new review.",
		task.HeadSHA, subject.reviewedHead, subject.reviewedBase)
}

// reentryPullMerged reports whether the latest observation of the cycle's
// pull request saw it merged. A merged pull request has nothing left for a
// re-entry to make ready, so the cycle ends instead of raising a hold the
// merge already answered; completion and foreclosure belong to the
// reconciler.
func (w *productionPublicationWorkflow) reentryPullMerged(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	var merged bool
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		predecessor, err := tx.GetReadyItemPRBinding(ctx, task.Successor.PredecessorItemID)
		if err != nil {
			return err
		}
		fact, err := tx.LatestPullMergeFact(ctx, predecessor.RepositoryID, predecessor.PRNumber)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		merged = fact.Merged
		return nil
	})
	return merged, err
}

const reentryPullMergedReason = "Superseded because the pull request merged while readiness was re-entering; the merge decides completion."

// endReentryCycleQuietly retires a re-entered cycle that has nothing left to
// raise: it supersedes the cycle's open hold, concludes a ready item an
// earlier pass of this cycle wrote, clears the run hold, and finishes the
// task. record, when set, commits in the same transaction, so the verdict
// that ended the cycle and the cleared hold land together.
func (w *productionPublicationWorkflow) endReentryCycleQuietly(
	ctx context.Context, task productionPublicationTask, reason string, record func(*store.InternalTx) error,
) error {
	if err := w.supersedeBlockedHold(ctx, task, reason, nil); err != nil {
		return err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if record != nil {
			if err := record(&tx.InternalTx); err != nil {
				return err
			}
		}
		ready, err := tx.GetAttentionItem(ctx, task.readyItemID())
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case ready.Status == domain.StatusOpen:
			ready.ItemVersion++
			ready, err = ready.ConcludeByDaemon(domain.ConcludedBySupersession, w.now().UTC(), reason, nil)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, ready); err != nil {
				return err
			}
		}
		return tx.ClearRunHold(ctx, task.RunID)
	}); err != nil {
		return err
	}
	return w.finishTask(ctx, task)
}
