package signet

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// applyCodexReenrollmentRecovery concludes the revoked-identity marker and
// appends its verified-operation-bound transition in the accepting command
// transaction. The store re-gates the latest journal outcome before commit.
func (s *Service) applyCodexReenrollmentRecovery(
	ctx context.Context, tx *store.WriteTx,
	command domain.Command, item domain.AttentionItem, status domain.ItemStatus,
) error {
	if item.CodexReenrollmentRecoveryBinding == nil {
		return domain.ErrCodexReenrollmentBindingMissing
	}
	now := s.now().UTC()
	if err := concludeItem(ctx, tx, item, status, now); err != nil {
		return err
	}
	binding := *item.CodexReenrollmentRecoveryBinding
	commandID := command.CommandID
	return tx.RecordCodexReenrollmentRecoveryTransition(ctx,
		domain.CodexReenrollmentRecoveryTransition{
			AuthIdentityID: binding.AuthIdentityID, LeaseFence: binding.LeaseFence,
			AuthStoreDigest:      binding.AuthStoreDigest,
			AccessTokenExpiresAt: binding.AccessTokenExpiresAt,
			CommandID:            &commandID,
			Reason: fmt.Sprintf("resolve_reenrollment accepted on item %s from device %s",
				item.ID, command.DeviceID),
			OccurredAt: now,
		})
}
