package signet

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func canonicalAlternativeChoices(choices []AlternativeChoice) (string, error) {
	canonical := slices.Clone(choices)
	sort.Slice(canonical, func(left, right int) bool {
		return canonical[left].FindingID < canonical[right].FindingID
	})
	for index, choice := range canonical {
		if choice.FindingID == "" || !slices.Contains(domain.AllAdjudicationRoutes, choice.Route) {
			return "", ErrInvalidFindingAdjudicationDecisionPayload
		}
		if index > 0 && canonical[index-1].FindingID == choice.FindingID {
			return "", ErrInvalidFindingAdjudicationDecisionPayload
		}
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidFindingAdjudicationDecisionPayload, err)
	}
	return string(body), nil
}

func decodeAlternativeChoices(message string) ([]AlternativeChoice, error) {
	var choices []AlternativeChoice
	if err := strictjson.Decode([]byte(message), &choices,
		strictjson.RejectInvalidUTF8, domain.MaxFindingAdjudicationBytes); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFindingAdjudicationDecisionPayload, err)
	}
	if len(choices) == 0 {
		return nil, ErrInvalidFindingAdjudicationDecisionPayload
	}
	canonical, err := canonicalAlternativeChoices(choices)
	if err != nil {
		return nil, err
	}
	if canonical != message {
		return nil, ErrInvalidFindingAdjudicationDecisionPayload
	}
	return choices, nil
}

func validateFindingAdjudicationDecision(
	command domain.Command, item domain.AttentionItem,
) error {
	if err := command.Validate(); err != nil {
		return fmt.Errorf("%w: command: %w",
			ErrInvalidFindingAdjudicationDecisionPayload, err)
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("%w: item: %w",
			ErrInvalidFindingAdjudicationDecisionPayload, err)
	}
	if item.FindingAdjudication == nil || item.Status != domain.StatusOpen || item.DecidedAt != nil ||
		command.ItemID != item.ID || !command.BindsSameAs(item) ||
		!item.Offers(command.Action) {
		return ErrInvalidFindingAdjudicationDecisionPayload
	}
	switch command.Action {
	case domain.ActionAcceptRecommendedRoute:
		if command.Message != "" || len(command.Attachments) > 0 {
			return ErrInvalidFindingAdjudicationDecisionPayload
		}
	case domain.ActionChooseAlternativeRoute:
		choices, err := decodeAlternativeChoices(command.Message)
		if err != nil {
			return err
		}
		proposals := make(map[domain.FindingID]domain.FindingAdjudicationProposal,
			len(item.FindingAdjudication.Proposals))
		for _, proposal := range item.FindingAdjudication.Proposals {
			proposals[proposal.FindingID] = proposal
		}
		// The offered set consulted here is authenticated transitively: the
		// caller loads the item through the store's re-gating snapshot read
		// (Service.Submit's GetAttentionItemSnapshot, and PutCommand's
		// GetAttentionItem), which binds proposal.OfferedAlternatives to the
		// digest-bound artifact (#893). A route present only in a tampered item
		// payload fails that read before reaching this check, so accepting a route
		// from the item's offered set cannot authorize an unoffered choice.
		for _, choice := range choices {
			proposal, ok := proposals[choice.FindingID]
			if !ok {
				return ErrAlternativeNotOffered
			}
			offered := slices.ContainsFunc(proposal.OfferedAlternatives, func(alternative domain.OfferedAlternative) bool {
				return alternative.Route == choice.Route
			})
			if !offered {
				return ErrAlternativeNotOffered
			}
		}
	default:
		return ErrInvalidFindingAdjudicationDecisionPayload
	}
	return nil
}

func (s *Service) applyFindingAdjudicationDecision(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	status domain.ItemStatus,
) error {
	if err := validateFindingAdjudicationDecision(command, item); err != nil {
		return err
	}
	return concludeItem(ctx, tx, item, status, s.now().UTC())
}
