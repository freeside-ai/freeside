package signet

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// applyReviewConfigurationRecovery concludes the displayed configuration item
// and appends its exact-row authorization in the same accepting transaction
// as the command. The adoption target is resolved here, at decision time, as
// the repository's currently approved (latest activated) profile revision:
// the item's immutable binding cannot carry it, because the approved revision
// legitimately advances while the item is parked (the operator typically
// re-onboards the profile and then decides). The store re-gates the command,
// item binding, immutable failure, and the review-configuration-only profile
// supersession before accepting the transition, so a stale or forged carrier
// rolls the whole decision back; the engine re-derives the same gate plus the
// effective-configuration equality on every read.
func (s *Service) applyReviewConfigurationRecovery(
	ctx context.Context, tx *store.WriteTx,
	command domain.Command, item domain.AttentionItem, status domain.ItemStatus,
) error {
	if item.ReviewConfigurationRecovery == nil {
		return domain.ErrReviewConfigRecoveryBindingMissing
	}
	now := s.now().UTC()
	if err := concludeItem(ctx, tx, item, status, now); err != nil {
		return err
	}
	binding := *item.ReviewConfigurationRecovery
	superseding, err := tx.LatestTrustProfile(ctx, binding.Repo)
	if err != nil {
		return fmt.Errorf("resolve adoption target for %q: %w", binding.Repo, err)
	}
	// Decision-time effectiveness gate (issue #611, Codex round 4): a target
	// that does not approve the daemon's effective configuration could never
	// grant authority, and accepting it would conclude the item while the
	// unique one-adoption-per-failure binding blocks any corrected retry, a
	// permanent operator dead-end. Rejecting rolls the whole transaction back
	// so the item stays open and the operator adopts again after activating
	// the matching revision. Authority remains engine-derived on every read;
	// this gate only refuses to record a decision that cannot work.
	if s.effectiveReviewConfig != nil {
		if effective := s.effectiveReviewConfig(); effective != "" &&
			(superseding.Review.Mode != domain.ReviewFreesideInvoked ||
				superseding.Review.ConfigDigest != effective) {
			return fmt.Errorf("adoption target %s: %w",
				superseding.ProfileDigest, domain.ErrReviewConfigAdoptionIneffective)
		}
	}
	commandID := command.CommandID
	return tx.RecordReviewConfigurationRecoveryTransition(ctx, domain.ReviewConfigurationRecoveryTransition{
		RunID: binding.RunID, InvocationID: binding.InvocationID, Round: binding.Round,
		BaseSHA: binding.BaseSHA, HeadSHA: binding.HeadSHA,
		FailureDigest: binding.FailureDigest,
		Repo:          binding.Repo, RepositoryID: binding.RepositoryID,
		SupersededProfileDigest:  binding.SupersededProfileDigest,
		SupersedingProfileDigest: superseding.ProfileDigest,
		CommandID:                &commandID,
		Reason: fmt.Sprintf("adopt_review_configuration accepted on item %s from device %s",
			item.ID, command.DeviceID),
		OccurredAt: now,
	})
}
