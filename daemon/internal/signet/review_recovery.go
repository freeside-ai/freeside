package signet

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// applyReviewRecovery concludes the displayed contradiction item and appends
// its exact-row authorization in the same accepting transaction as the
// command. The store re-gates the command, item binding, and immutable failure
// before accepting the transition, so a stale or forged carrier rolls the
// whole decision back.
func (s *Service) applyReviewRecovery(
	ctx context.Context, tx *store.WriteTx,
	command domain.Command, item domain.AttentionItem, status domain.ItemStatus,
) error {
	if item.ReviewRecoveryBinding == nil {
		return domain.ErrReviewRecoveryBindingMissing
	}
	now := s.now().UTC()
	if err := concludeItem(ctx, tx, item, status, now); err != nil {
		return err
	}
	binding := *item.ReviewRecoveryBinding
	commandID := command.CommandID
	return tx.RecordReviewRecoveryTransition(ctx, domain.ReviewRecoveryTransition{
		RunID: binding.RunID, InvocationID: binding.InvocationID, Round: binding.Round,
		BaseSHA: binding.BaseSHA, HeadSHA: binding.HeadSHA,
		FailureDigest: binding.FailureDigest, CommandID: &commandID,
		Reason: fmt.Sprintf("recover_review accepted on item %s from device %s",
			item.ID, command.DeviceID),
		OccurredAt: now,
	})
}
