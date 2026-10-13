package signet

import (
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var (
	ErrInvalidReviewDisputeDecisionPayload = errors.New("invalid review dispute decision payload")
	ErrFindingRouteNotOffered              = errors.New("finding route is not offered")
	ErrFindingRoutesIncomplete             = errors.New("finding routes must cover every disputed finding")
)

func validateReviewDisputeDecision(command domain.Command, item domain.AttentionItem) error {
	if command.Action != domain.ActionAdjudicateFindings || item.Type != domain.AttentionReviewDispute ||
		item.ReviewDispute == nil || item.Status != domain.StatusOpen || item.DecidedAt != nil ||
		command.ItemID != item.ID || !command.BindsSameAs(item) || !item.Offers(command.Action) ||
		len(command.Attachments) != 0 {
		return ErrInvalidReviewDisputeDecisionPayload
	}
	choices, err := decodeAlternativeChoices(command.Message)
	if err != nil {
		return errors.Join(ErrInvalidReviewDisputeDecisionPayload, err)
	}
	if len(choices) != len(item.ReviewDispute.Findings) {
		return ErrFindingRoutesIncomplete
	}
	for _, choice := range choices {
		index := slices.IndexFunc(item.ReviewDispute.Findings, func(row domain.DisputedFinding) bool { return row.FindingID == choice.FindingID })
		if index < 0 || !slices.Contains(item.ReviewDispute.Findings[index].OfferedRoutes, choice.Route) {
			return ErrFindingRouteNotOffered
		}
	}
	return nil
}
