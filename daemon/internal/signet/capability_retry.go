package signet

import (
	"context"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func validateCapabilityRetryDecision(command domain.Command, item domain.AttentionItem) error {
	if err := command.Validate(); err != nil {
		return fmt.Errorf("%w: command: %w", ErrInvalidCapabilityRetryDecisionPayload, err)
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("%w: item: %w", ErrInvalidCapabilityRetryDecisionPayload, err)
	}
	if item.Type != domain.AttentionExecutionFailure || item.ExecutionFailure == nil ||
		item.ExecutionFailure.Stage != domain.StageNameImplementation ||
		item.Status != domain.StatusOpen || item.DecidedAt != nil ||
		command.Action != domain.ActionRetryWithCapability || command.ItemID != item.ID ||
		!command.BindsSameAs(item) || !item.Offers(command.Action) ||
		len(command.Attachments) != 0 || !contentaddr.Valid(command.Message) {
		return ErrInvalidCapabilityRetryDecisionPayload
	}
	offered := slices.ContainsFunc(
		item.ExecutionFailure.OfferedManifests,
		func(offer domain.CapabilityManifestOffer) bool {
			return string(offer.Digest) == command.Message
		},
	)
	if !offered {
		return ErrCapabilityManifestNotOffered
	}
	return nil
}

func (s *Service) applyCapabilityRetryDecision(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	status domain.ItemStatus,
) error {
	if err := validateCapabilityRetryDecision(command, item); err != nil {
		return err
	}
	return concludeItem(ctx, tx, item, status, s.now().UTC())
}
