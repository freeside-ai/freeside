package signet

import (
	"context"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// validateAlternateAgentRetryDecision holds a retry_with_alternate_agent
// decision to the card it decides: an open, undecided implementation-stage
// execution failure that offers the action, and an agent among the card's
// offers. Whether the agent still qualifies is the engine's to re-derive
// (AlternateAgentRetrier).
func validateAlternateAgentRetryDecision(command domain.Command, item domain.AttentionItem) error {
	if err := command.Validate(); err != nil {
		return fmt.Errorf("%w: command: %w", ErrInvalidAlternateAgentDecisionPayload, err)
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("%w: item: %w", ErrInvalidAlternateAgentDecisionPayload, err)
	}
	if item.Type != domain.AttentionExecutionFailure || item.ExecutionFailure == nil ||
		item.ExecutionFailure.Stage != domain.StageNameImplementation ||
		item.Status != domain.StatusOpen || item.DecidedAt != nil ||
		command.Action != domain.ActionRetryWithAlternateAgent || command.ItemID != item.ID ||
		!command.BindsSameAs(item) || !item.Offers(command.Action) ||
		len(command.Attachments) != 0 || command.Message == "" {
		return ErrInvalidAlternateAgentDecisionPayload
	}
	if !slices.ContainsFunc(item.AlternateAgents, func(offer domain.AlternateAgentOffer) bool {
		return offer.Agent == command.Message
	}) {
		return ErrAlternateAgentNotOffered
	}
	return nil
}

// applyAlternateAgentRetryDecision records the choice and concludes the card
// in the accepting transaction, so the record never exists without its
// decision nor the decision without its record.
func (s *Service) applyAlternateAgentRetryDecision(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	status domain.ItemStatus,
) error {
	if err := validateAlternateAgentRetryDecision(command, item); err != nil {
		return err
	}
	if s.alternateAgentRetrier == nil {
		return ErrOperatorSettingUnavailable
	}
	now := s.now().UTC()
	if _, err := s.alternateAgentRetrier(ctx, tx, command, item, now); err != nil {
		return err
	}
	return concludeItem(ctx, tx, command, item, status, now)
}
