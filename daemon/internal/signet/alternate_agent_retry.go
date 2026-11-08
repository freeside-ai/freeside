package signet

import (
	"context"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The wire still names alternates by tree agent (the decision's
// alternate_agent, a move_parked_task's agent, and each offer's agent) while
// the daemon stores enrollments and selections (#2735). agentNames
// translates between the two at the handler and the projection; #2808 moves
// the wire onto selections and removes it.
type agentNames struct {
	// tree is the committed agent tree, nil when the daemon holds none,
	// which translates nothing.
	tree *agenttree.Tree
}

// agentNames is the translation over the daemon's committed tree.
func (s *Service) agentNames() agentNames {
	if s.agentTreeFunc == nil {
		return agentNames{}
	}
	return agentNames{tree: s.agentTreeFunc()}
}

// selection is the enrollment, model, and effort the named tree agent
// reduces to (agenttree.Tree.SelectionOfAgent), or false when there is no
// tree or the tree reduces no such agent.
func (n agentNames) selection(name string) (domain.TaskLineSelection, bool) {
	if n.tree == nil {
		return domain.TaskLineSelection{}, false
	}
	selection, err := n.tree.SelectionOfAgent(name)
	return selection, err == nil
}

// choiceMessage is a retry_with_alternate_agent's durable message for the
// tree agent the wire names: the choice of the selection it reduces to
// (domain.AlternateAgentChoice.Message). A name no tree agent reduces is not
// offered; the refusal never repeats it.
func (n agentNames) choiceMessage(name string) (string, error) {
	selection, ok := n.selection(name)
	if !ok {
		return "", ErrAlternateAgentNotOffered
	}
	return domain.AlternateAgentChoice(selection).Message(), nil
}

// renderOffers is offers as the wire names them: each enrollment offer under
// the tree agent that runs the implementer's lineup model and effort on that
// enrollment (agenttree.Tree.AgentForSelection), sorted by agent. An offer no
// one agent maps to is dropped, so before #2808 the operator may see fewer
// offers than the daemon derived. A pre-#2735 offer already names its agent.
// It returns nil when no offer is left.
func (n agentNames) renderOffers(offers []domain.AlternateAgentOffer) []domain.AlternateAgentOffer {
	var rendered []domain.AlternateAgentOffer
	for _, offer := range offers {
		if offer.Agent == "" {
			name, ok := n.offerAgent(offer)
			if !ok {
				continue
			}
			offer.Agent, offer.Enrollment = name, ""
		}
		rendered = append(rendered, offer)
	}
	domain.SortAlternateAgentOffers(rendered)
	return rendered
}

// offerAgent is the tree agent an enrollment offer renders as, or false when
// there is no tree, no implementer lineup line, or no one agent for it.
func (n agentNames) offerAgent(offer domain.AlternateAgentOffer) (string, bool) {
	if n.tree == nil {
		return "", false
	}
	lineup, err := n.tree.ResolveLineup()
	if err != nil {
		return "", false
	}
	line, ok := lineup.Line(domain.RoleImplementer)
	if !ok {
		return "", false
	}
	name, err := n.tree.AgentForSelection(domain.RoleImplementer, domain.TaskLineSelection{
		EnrollmentID: offer.Enrollment, ModelID: line.Names.Model, Effort: line.Names.Effort,
	})
	return name, err == nil
}

// renderItem is the item with its offers as the wire names them. When no
// offer is left the item renders with no offers and without the
// retry_with_alternate_agent action, which the wire offers exactly beside
// them; the stored item is unchanged and still offers it.
func (n agentNames) renderItem(item domain.AttentionItem) domain.AttentionItem {
	if item.AlternateAgents == nil {
		return item
	}
	item.AlternateAgents = n.renderOffers(item.AlternateAgents)
	if item.AlternateAgents == nil {
		item.RequestedDecision = slices.DeleteFunc(slices.Clone(item.RequestedDecision), func(action domain.Action) bool {
			return action == domain.ActionRetryWithAlternateAgent
		})
	}
	return item
}

// renderHold is the hold with its quota window's offers as the wire names
// them.
func (n agentNames) renderHold(hold *domain.RunHoldObservation) *domain.RunHoldObservation {
	if hold == nil || hold.QuotaWindow == nil || hold.QuotaWindow.AlternateAgents == nil {
		return hold
	}
	window := *hold.QuotaWindow
	window.AlternateAgents = n.renderOffers(window.AlternateAgents)
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
