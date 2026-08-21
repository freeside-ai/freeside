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

// FindingAdjudicationDecision is the typed durable decision #840 consumes.
// The command itself remains the authority for item version and artifact
// bindings; this shape carries only the selected routing semantics.
type FindingAdjudicationDecision struct {
	Action             domain.Action
	AlternativeChoices []AlternativeChoice
}

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

// DecodeFindingAdjudicationDecision reconstructs one accepted typed decision
// and revalidates it against either the exact open item used at acceptance or
// its one-version resolved successor, plus the offered-alternative set.
func DecodeFindingAdjudicationDecision(
	command domain.Command, item domain.AttentionItem,
) (FindingAdjudicationDecision, error) {
	if err := command.Validate(); err != nil {
		return FindingAdjudicationDecision{}, fmt.Errorf("%w: command: %w",
			ErrInvalidFindingAdjudicationDecisionPayload, err)
	}
	if err := item.Validate(); err != nil {
		return FindingAdjudicationDecision{}, fmt.Errorf("%w: item: %w",
			ErrInvalidFindingAdjudicationDecisionPayload, err)
	}
	if item.FindingAdjudication == nil || !commandBindsAdjudicationItem(command, item) ||
		!item.Offers(command.Action) {
		return FindingAdjudicationDecision{}, ErrInvalidFindingAdjudicationDecisionPayload
	}
	decision := FindingAdjudicationDecision{Action: command.Action}
	switch command.Action {
	case domain.ActionAcceptRecommendedRoute:
		if command.Message != "" || len(command.Attachments) > 0 {
			return FindingAdjudicationDecision{}, ErrInvalidFindingAdjudicationDecisionPayload
		}
	case domain.ActionChooseAlternativeRoute:
		choices, err := decodeAlternativeChoices(command.Message)
		if err != nil {
			return FindingAdjudicationDecision{}, err
		}
		proposals := make(map[domain.FindingID]domain.FindingAdjudicationProposal,
			len(item.FindingAdjudication.Proposals))
		for _, proposal := range item.FindingAdjudication.Proposals {
			proposals[proposal.FindingID] = proposal
		}
		for _, choice := range choices {
			proposal, ok := proposals[choice.FindingID]
			if !ok {
				return FindingAdjudicationDecision{}, ErrAlternativeNotOffered
			}
			offered := slices.ContainsFunc(proposal.OfferedAlternatives, func(alternative domain.OfferedAlternative) bool {
				return alternative.Route == choice.Route
			})
			if !offered {
				return FindingAdjudicationDecision{}, ErrAlternativeNotOffered
			}
		}
		decision.AlternativeChoices = choices
	default:
		return FindingAdjudicationDecision{}, ErrInvalidFindingAdjudicationDecisionPayload
	}
	return decision, nil
}

func commandBindsAdjudicationItem(command domain.Command, item domain.AttentionItem) bool {
	if command.ItemID != item.ID || command.PRHeadSHA != item.PRHeadSHA ||
		!slices.Equal(command.ArtifactDigests, item.ArtifactDigests) {
		return false
	}
	if command.ItemVersion == item.ItemVersion {
		return item.Status == domain.StatusOpen && item.DecidedAt == nil
	}
	return item.Status == domain.StatusResolved && item.DecidedAt != nil &&
		item.ItemVersion > 1 && command.ItemVersion == item.ItemVersion-1
}

func (s *Service) applyFindingAdjudicationDecision(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	status domain.ItemStatus,
) error {
	if _, err := DecodeFindingAdjudicationDecision(command, item); err != nil {
		return err
	}
	return concludeItem(ctx, tx, item, status, s.now().UTC())
}
