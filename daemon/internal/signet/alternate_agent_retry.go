package signet

import (
	"context"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// AlternateAgentChoice mirrors the API's AlternateAgentChoice: an offered
// enrollment and, where the operator picks them, a model and an effort. A nil
// model or effort is the implementer lineup line's.
type AlternateAgentChoice struct {
	EnrollmentID domain.ClientEnrollmentID `json:"enrollment_id"`
	ModelID      *string                   `json:"model_id"`
	Effort       *string                   `json:"effort"`
}

// choice is the domain choice the wire names. The wire's null, not an empty
// string, means the lineup's, so a present empty model or effort is refused
// like an invalid one: the error names the field, never the text.
func (c AlternateAgentChoice) choice() (domain.AlternateAgentChoice, error) {
	choice := domain.AlternateAgentChoice{EnrollmentID: c.EnrollmentID}
	for _, field := range []struct {
		name  string
		wire  *string
		value *string
	}{{"model_id", c.ModelID, &choice.ModelID}, {"effort", c.Effort, &choice.Effort}} {
		if field.wire == nil {
			continue
		}
		if *field.wire == "" {
			return domain.AlternateAgentChoice{}, fmt.Errorf("alternate agent choice %s: %w",
				field.name, domain.ErrInvalidTaskLineSelection)
		}
		*field.value = *field.wire
	}
	if err := choice.Validate(); err != nil {
		return domain.AlternateAgentChoice{}, err
	}
	return choice, nil
}

// wireAlternateAgentChoice is a stored choice as the wire carries it, null
// for a model or effort the choice left to the lineup.
func wireAlternateAgentChoice(choice domain.AlternateAgentChoice) AlternateAgentChoice {
	optional := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	return AlternateAgentChoice{
		EnrollmentID: choice.EnrollmentID, ModelID: optional(choice.ModelID), Effort: optional(choice.Effort),
	}
}

// renderOffers is offers as the wire carries them. A stored set is all
// enrollment offers or all pre-#2735 offers (domain.ValidateAlternateAgentOffers),
// already sorted; a pre-#2735 set names tree agents the wire no longer
// carries and renders as nil.
func renderOffers(offers []domain.AlternateAgentOffer) []domain.AlternateAgentOffer {
	if len(offers) == 0 || offers[0].Agent != "" {
		return nil
	}
	return offers
}

// renderItem is the item with its offers as the wire carries them. When no
// offer is left the item renders with no offers and without the
// retry_with_alternate_agent action, which the wire offers exactly beside
// them; the stored item is unchanged and still offers it.
func renderItem(item domain.AttentionItem) domain.AttentionItem {
	if item.AlternateAgents == nil {
		return item
	}
	item.AlternateAgents = renderOffers(item.AlternateAgents)
	if item.AlternateAgents == nil {
		item.RequestedDecision = slices.DeleteFunc(slices.Clone(item.RequestedDecision), func(action domain.Action) bool {
			return action == domain.ActionRetryWithAlternateAgent
		})
	}
	return item
}

// renderHold is the hold with its quota window's offers as the wire carries
// them.
func renderHold(hold *domain.RunHoldObservation) *domain.RunHoldObservation {
	if hold == nil || hold.QuotaWindow == nil || hold.QuotaWindow.AlternateAgents == nil {
		return hold
	}
	window := *hold.QuotaWindow
	window.AlternateAgents = renderOffers(window.AlternateAgents)
	rendered := *hold
	rendered.QuotaWindow = &window
	return &rendered
}

// validateAlternateAgentRetryDecision holds a retry_with_alternate_agent
// decision to the card it decides: an open, undecided implementation-stage
// execution failure that offers the action, and a choice
// (domain.ParseAlternateAgentChoice) of an enrollment among the card's
// offers. Whether it still qualifies, and whether the enrollment serves the
// model and effort, is the engine's to derive (AlternateAgentRetrier).
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
	choice, err := domain.ParseAlternateAgentChoice(command.Message)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAlternateAgentDecisionPayload, err)
	}
	if _, ok := domain.OfferedEnrollment(item.AlternateAgents, choice.EnrollmentID); !ok {
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
