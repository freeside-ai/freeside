package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func (tx *ReadTx) gateExecutableReviewDispute(ctx context.Context, item domain.AttentionItem) error {
	binding := item.ReviewDispute
	if item.ID != domain.ProductionFindingAdjudicationItemID(binding.RunID, binding.Round, 1) {
		return domain.ErrParentKeyMismatch
	}
	record, err := tx.reviewRecordForRound(ctx, binding.RunID, binding.Round)
	if err != nil {
		return err
	}
	if record.CompletionEvidence != binding.CompletionEvidence || item.PRHeadSHA != record.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	for _, id := range record.FindingIDs {
		if !slices.Contains(binding.FindingIDs, id) {
			return domain.ErrParentKeyMismatch
		}
	}
	var external []domain.FindingID
	for _, id := range binding.FindingIDs {
		if !slices.Contains(record.FindingIDs, id) {
			external = append(external, id)
		}
	}
	if len(external) > 0 {
		if err := tx.requireAdmittedExternalEntries(ctx, record, external); err != nil {
			return err
		}
	}
	if record.Outcome != domain.ReviewFindings && (record.Outcome != domain.ReviewClean || len(external) != len(binding.FindingIDs)) {
		return domain.ErrParentKeyMismatch
	}
	policy, err := tx.GetResolvedPolicy(ctx, binding.RunID)
	if err != nil {
		return err
	}
	for _, row := range binding.Findings {
		finding, err := tx.GetFinding(ctx, row.FindingID)
		if err != nil {
			return err
		}
		var identity *domain.DisputedFindingExternal
		if finding.External != nil {
			identity = &domain.DisputedFindingExternal{ReviewerLogin: finding.External.ReviewerLogin, ThreadID: finding.External.ThreadID}
		}
		if row.Message != finding.Message || !reflect.DeepEqual(row.Location, finding.Location) || row.Severity != finding.Severity ||
			!reflect.DeepEqual(row.External, identity) || !slices.Equal(row.OfferedRoutes, domain.ReviewDisputeRoutes(finding, domain.CanonicalDeclaredPaths(policy))) {
			return domain.ErrParentKeyMismatch
		}
	}
	if len(binding.Findings) < len(binding.FindingIDs) {
		prior, err := tx.firstNonHumanFindingAdjudication(ctx, binding.RunID, binding.Round)
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		if err != nil {
			return err
		}
		for _, id := range binding.FindingIDs {
			listed := slices.ContainsFunc(binding.Findings, func(row domain.DisputedFinding) bool { return row.FindingID == id })
			if !listed && !slices.ContainsFunc(prior.Entries, func(entry domain.FindingAdjudicationEntry) bool { return entry.FindingID == id }) {
				return domain.ErrParentKeyMismatch
			}
		}
	}
	return nil
}

// ReviewDisputeDecision reconstructs the terminal decision independently of
// later delivery version advances. Discuss never concludes the dispute.
func (tx *ReadTx) ReviewDisputeDecision(ctx context.Context, id domain.ItemID) (domain.AttentionItem, *domain.Command, error) {
	item, err := tx.GetAttentionItem(ctx, id)
	if err != nil {
		return domain.AttentionItem{}, nil, err
	}
	if item.Type != domain.AttentionReviewDispute {
		return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
	}
	commands, err := tx.ListCommandsForItem(ctx, id)
	if err != nil {
		return domain.AttentionItem{}, nil, err
	}
	var terminal *domain.Command
	for index := range commands {
		command := &commands[index]
		switch command.Action {
		case domain.ActionAdjudicateFindings, domain.ActionStop:
			if terminal != nil {
				return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
			}
			terminal = command
		case domain.ActionDiscuss:
			continue
		default:
			return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
		}
	}
	if item.Status == domain.StatusOpen && terminal == nil {
		return item, nil, nil
	}
	if terminal == nil || item.Status != domain.StatusResolved || item.DecidedAt == nil ||
		terminal.ItemVersion >= item.ItemVersion || terminal.ItemID != item.ID || !item.Offers(terminal.Action) ||
		terminal.PRHeadSHA != item.PRHeadSHA || !slices.Equal(terminal.ArtifactDigests, item.ArtifactDigests) {
		return domain.AttentionItem{}, nil, domain.ErrParentKeyMismatch
	}
	return item, terminal, nil
}

func (tx *ReadTx) validateHumanAdjudicationDecision(ctx context.Context, artifact domain.FindingAdjudication) error {
	decision := artifact.Decision
	item, command, err := tx.ReviewDisputeDecision(ctx, decision.ItemID)
	if err != nil {
		return err
	}
	if command == nil || command.Action != domain.ActionAdjudicateFindings || command.CommandID != decision.CommandID ||
		command.ItemVersion != decision.ItemVersion || item.ReviewDispute == nil || item.ReviewDispute.RunID != artifact.RunID ||
		item.ReviewDispute.Round != artifact.Round || !artifact.CreatedAt.Equal(*item.DecidedAt) || len(command.Attachments) != 0 {
		return domain.ErrParentKeyMismatch
	}
	type choice struct {
		FindingID domain.FindingID         `json:"finding_id"`
		Route     domain.AdjudicationRoute `json:"route"`
	}
	var choices []choice
	if err := strictjson.Decode([]byte(command.Message), &choices, strictjson.RejectInvalidUTF8, domain.MaxFindingAdjudicationBytes); err != nil {
		return err
	}
	slices.SortFunc(choices, func(a, b choice) int { return strings.Compare(string(a.FindingID), string(b.FindingID)) })
	body, err := json.Marshal(choices)
	if err != nil {
		return err
	}
	if string(body) != command.Message || len(choices) != len(item.ReviewDispute.Findings) {
		return domain.ErrParentKeyMismatch
	}
	var prior *domain.FindingAdjudication
	if artifact.Revision > 1 {
		if artifact.PredecessorDigest == nil {
			return domain.ErrParentKeyMismatch
		}
		got, err := tx.GetFindingAdjudication(ctx, *artifact.PredecessorDigest)
		if err != nil {
			return err
		}
		prior = &got
		if prior.RunID != artifact.RunID || prior.Round != artifact.Round || prior.Revision+1 != artifact.Revision ||
			prior.FindingBatchDigest != artifact.FindingBatchDigest || prior.ApprovedSpecDigest != artifact.ApprovedSpecDigest ||
			prior.InstructionSnapshotDigest != artifact.InstructionSnapshotDigest || prior.ResolvedPolicyDigest != artifact.ResolvedPolicyDigest {
			return domain.ErrParentKeyMismatch
		}
	}
	chosen := make(map[domain.FindingID]domain.AdjudicationRoute, len(choices))
	for _, choice := range choices {
		if _, exists := chosen[choice.FindingID]; exists {
			return domain.ErrParentKeyMismatch
		}
		row := slices.IndexFunc(item.ReviewDispute.Findings, func(row domain.DisputedFinding) bool { return row.FindingID == choice.FindingID })
		if row < 0 || !slices.Contains(item.ReviewDispute.Findings[row].OfferedRoutes, choice.Route) {
			return domain.ErrParentKeyMismatch
		}
		chosen[choice.FindingID] = choice.Route
	}
	for _, entry := range artifact.Entries {
		if route, ok := chosen[entry.FindingID]; ok {
			expected, err := domain.NewHumanAdjudicationEntry(entry.FindingID, route)
			if err != nil || !reflect.DeepEqual(expected, entry) {
				return domain.ErrParentKeyMismatch
			}
			delete(chosen, entry.FindingID)
		} else {
			if prior == nil {
				return domain.ErrParentKeyMismatch
			}
			index := slices.IndexFunc(prior.Entries, func(old domain.FindingAdjudicationEntry) bool { return old.FindingID == entry.FindingID })
			if index < 0 || !reflect.DeepEqual(prior.Entries[index], entry) {
				return domain.ErrParentKeyMismatch
			}
		}
	}
	if len(chosen) != 0 {
		return domain.ErrParentKeyMismatch
	}
	return nil
}
