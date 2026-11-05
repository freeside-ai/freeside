package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reviewRoundDiffMetrics computes the diff metrics for the round record is
// about to become (plan §7 Review Drift). A nil result is a gap: the round is
// recorded without metrics and the growth rule skips it. Metrics are never
// backfilled, so a gap is permanent. A count that cannot be computed is
// therefore a gap only when retrying could not help; a cancelled context or an
// environmental failure is an error, and the pass retries before the record
// is written.
func (w *productionPublicationWorkflow) reviewRoundDiffMetrics(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	reviewWorkspace string,
	record domain.ReviewRecord,
) (*domain.ReviewRoundDiffMetrics, error) {
	if task.reentersInPlace() {
		// The round reviewed commits Freeside did not produce, possibly a
		// prospective merge the record does not name. Counting the record's
		// base to its head would not describe what was reviewed, so the
		// round keeps the gap an uncountable round already has.
		return nil, nil
	}
	var previous *domain.ReviewRecord
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		latest, err := tx.LatestReviewRecord(ctx, task.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		previous = &latest
		return err
	}); err != nil {
		return nil, err
	}
	if previous != nil && previous.Round >= record.Round {
		return nil, nil
	}
	cumulative, err := deriveDiffStats(
		ctx, w.workDir, reviewWorkspace, record.BaseSHA, record.HeadSHA)
	if err != nil {
		return nil, reviewRoundMetricsGap(ctx, err)
	}
	roundStats := cumulative
	if previous != nil {
		roundStats, err = w.diffStatsSincePreviousRound(
			ctx, task, binding, reviewWorkspace, *previous, record)
		if err != nil {
			return nil, reviewRoundMetricsGap(ctx, err)
		}
	}
	metrics := domain.ReviewRoundDiffMetrics{Cumulative: *cumulative, Round: *roundStats}
	if metrics.Validate() != nil {
		return nil, nil
	}
	return &metrics, nil
}

// reviewRoundMetricsGap returns nil when err is a deterministic refusal the
// round records as a gap, and the error to retry on otherwise.
func reviewRoundMetricsGap(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if productionPublicationRetryableFailure(err) {
		return err
	}
	return nil
}

// diffStatsSincePreviousRound counts previous.HeadSHA..record.HeadSHA. Each
// pass rebuilds the candidate as a fresh commit on the base, so a changed head
// leaves the previous one out of the review workspace. After a remediation its
// tree is rebuilt from the remediation input's candidate patch and checked
// against the tree the verification checkpoint recorded for that head. Any
// other changed head (an operator-feedback successor) is refused.
func (w *productionPublicationWorkflow) diffStatsSincePreviousRound(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	reviewWorkspace string,
	previous, record domain.ReviewRecord,
) (*domain.DiffStats, error) {
	if previous.HeadSHA == record.HeadSHA {
		return deriveDiffStats(ctx, w.workDir, reviewWorkspace, previous.HeadSHA, record.HeadSHA)
	}
	if binding.remediation == nil {
		return nil, fmt.Errorf("previous review head %q has no remediation input: %w",
			previous.HeadSHA, store.ErrNotFound)
	}
	request := binding.remediation.request
	if request.HeadSHA != previous.HeadSHA || request.BaseSHA != record.BaseSHA {
		return nil, fmt.Errorf("remediation input covers %q..%q, want %q..%q: %w",
			request.BaseSHA, request.HeadSHA, record.BaseSHA, previous.HeadSHA,
			domain.ErrParentKeyMismatch)
	}
	previousTree, err := w.loadRemediationSourceTree(ctx, task, binding, reviewWorkspace)
	if err != nil {
		return nil, err
	}
	input, _, err := loadRemediationInput(w.artifacts, request.InputArtifactDigest)
	if err != nil {
		return nil, err
	}
	if input.BaseSHA != request.BaseSHA || input.HeadSHA != request.HeadSHA {
		return nil, fmt.Errorf("remediation input names %q..%q: %w",
			input.BaseSHA, input.HeadSHA, domain.ErrParentKeyMismatch)
	}
	return deriveDiffStatsFromPatch(
		ctx, w.workDir, reviewWorkspace, request.BaseSHA, input.CandidatePatchBase64,
		previousTree, previous.HeadSHA, record.HeadSHA)
}
