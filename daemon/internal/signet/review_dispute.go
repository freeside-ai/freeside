package signet

import (
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
)

var (
	ErrInvalidReviewDisputeDecisionPayload = errors.New("invalid review dispute decision payload")
	ErrFindingRouteNotOffered              = errors.New("finding route is not offered")
	ErrFindingRoutesIncomplete             = errors.New("finding routes must cover every disputed finding without a settled route")
	ErrFindingRouteReasonInvalid           = errors.New("finding route reason is missing, not allowed, or invalid")
	ErrFindingRouteReasonUnpublishable     = errors.New("finding route reason fails publication screening")
)

func validateReviewDisputeDecision(command domain.Command, item domain.AttentionItem) error {
	if command.Action != domain.ActionAdjudicateFindings || item.Type != domain.AttentionReviewDispute ||
		item.ReviewDispute == nil || item.Status != domain.StatusOpen || item.DecidedAt != nil ||
		command.ItemID != item.ID || !command.BindsSameAs(item) || !item.Offers(command.Action) ||
		len(command.Attachments) != 0 {
		return ErrInvalidReviewDisputeDecisionPayload
	}
	choices, err := decodeFindingRoutes(command.Message)
	if err != nil {
		return errors.Join(ErrInvalidReviewDisputeDecisionPayload, err)
	}
	for _, choice := range choices {
		index := slices.IndexFunc(item.ReviewDispute.Findings, func(row domain.DisputedFinding) bool { return row.FindingID == choice.FindingID })
		if index < 0 || !slices.Contains(item.ReviewDispute.Findings[index].OfferedRoutes, choice.Route) {
			return ErrFindingRouteNotOffered
		}
		// A decline or defer of an external finding posts a reply in the
		// reviewer's thread, so the person says why; nothing else posts one.
		repliesExternally := item.ReviewDispute.Findings[index].External != nil &&
			(choice.Route == domain.RouteDecline || choice.Route == domain.RouteDefer)
		if repliesExternally != (choice.Reason != "") {
			return ErrFindingRouteReasonInvalid
		}
		// The reply screens the reason with this ruleset and, on failure,
		// refuses the reply for good. Screen it here too, while the person
		// can still rewrite it, so an accepted reason is one that posts.
		if repliesExternally {
			if err := publicationtext.ScreenIssueBody(domain.IssueTextRulesetGitHubIssue1, choice.Reason, domain.MaxHumanAdjudicationReasonBytes); err != nil {
				return fmt.Errorf("%w: %w", ErrFindingRouteReasonUnpublishable, err)
			}
		}
	}
	// A row with a settled route may go unanswered, which keeps that route;
	// an answer with any offered route overrides it. Every other row needs an
	// answer. decodeFindingRoutes already refused a finding answered twice.
	for _, row := range item.ReviewDispute.Findings {
		answered := slices.ContainsFunc(choices, func(choice FindingRouteChoice) bool { return choice.FindingID == row.FindingID })
		if row.SettledRoute == nil && !answered {
			return ErrFindingRoutesIncomplete
		}
	}
	return nil
}
