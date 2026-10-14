package engine

import (
	"context"
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func (w *productionPublicationWorkflow) routedReviewDisputeBinding(ctx context.Context, task productionPublicationTask, record domain.ReviewRecord, listed []domain.FindingID) (*domain.ReviewDisputeBinding, error) {
	binding := &domain.ReviewDisputeBinding{
		RunID: record.RunID, Round: record.Round,
		FindingIDs: slices.Clone(record.FindingIDs), CompletionEvidence: record.CompletionEvidence,
	}
	external, err := w.openExternalFindings(ctx, task, record)
	if err != nil {
		return nil, err
	}
	for _, finding := range external {
		if !slices.Contains(binding.FindingIDs, finding.ID) {
			binding.FindingIDs = append(binding.FindingIDs, finding.ID)
		}
	}
	slices.Sort(binding.FindingIDs)
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		if listed != nil {
			// Early external dispositions disappear from openExternalFindings.
			// Keep the whole stored batch bound even after those replies settle.
			artifact, err := tx.GetFindingAdjudicationForRound(ctx, record.RunID, record.Round)
			if err != nil {
				return err
			}
			for _, entry := range artifact.Entries {
				if !slices.Contains(binding.FindingIDs, entry.FindingID) {
					binding.FindingIDs = append(binding.FindingIDs, entry.FindingID)
				}
			}
			slices.Sort(binding.FindingIDs)
		}
		policy, err := tx.GetResolvedPolicy(ctx, record.RunID)
		if err != nil {
			return err
		}
		for _, id := range binding.FindingIDs {
			if listed != nil && !slices.Contains(listed, id) {
				continue
			}
			finding, err := tx.GetFinding(ctx, id)
			if err != nil {
				return err
			}
			row := domain.DisputedFinding{
				FindingID: id, Message: finding.Message, Location: finding.Location,
				Severity: finding.Severity, OfferedRoutes: domain.ReviewDisputeRoutes(finding, domain.CanonicalDeclaredPaths(policy)),
			}
			if finding.External != nil {
				row.External = &domain.DisputedFindingExternal{ReviewerLogin: finding.External.ReviewerLogin, ThreadID: finding.External.ThreadID}
			}
			binding.Findings = append(binding.Findings, row)
		}
		return nil
	})
	return binding, err
}

// Consume the decision before the parked-batch early return. The decision time
// and canonical entry order make a replay after either durable hook identical.
func (w *productionPublicationWorkflow) consumeReviewDisputeDecision(ctx context.Context, task productionPublicationTask, binding productionBinding, record domain.ReviewRecord) (*domain.FindingAdjudication, error) {
	id := domain.ProductionFindingAdjudicationItemID(task.RunID, record.Round, 1)
	var item domain.AttentionItem
	var command *domain.Command
	var prior *domain.FindingAdjudication
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if got.Type != domain.AttentionReviewDispute {
			return nil
		}
		item, command, err = tx.ReviewDisputeDecision(ctx, id)
		if err != nil || command == nil || command.Action != domain.ActionAdjudicateFindings {
			return err
		}
		artifact, err := tx.GetFindingAdjudicationForRound(ctx, record.RunID, record.Round)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		prior = &artifact
		return nil
	})
	if err != nil {
		return nil, err
	}
	if item.Status == domain.StatusOpen && !item.Offers(domain.ActionAdjudicateFindings) && !task.externalReviewFirstRound(record) {
		if err := w.putReviewAttention(ctx, task, record, item.Reason, domain.AttentionReviewDispute); err != nil {
			return nil, err
		}
	}
	if command == nil || command.Action != domain.ActionAdjudicateFindings {
		return nil, nil
	}
	if prior != nil && prior.Decision != nil && prior.Decision.CommandID == command.CommandID {
		return prior, nil
	}
	var choices []struct {
		FindingID domain.FindingID         `json:"finding_id"`
		Route     domain.AdjudicationRoute `json:"route"`
	}
	if err := strictjson.Decode([]byte(command.Message), &choices, strictjson.RejectInvalidUTF8, domain.MaxFindingAdjudicationBytes); err != nil {
		return nil, err
	}
	var entries []domain.FindingAdjudicationEntry
	if prior != nil {
		entries = slices.Clone(prior.Entries)
	}
	for _, choice := range choices {
		entry, err := domain.NewHumanAdjudicationEntry(choice.FindingID, choice.Route)
		if err != nil {
			return nil, err
		}
		index := slices.IndexFunc(entries, func(old domain.FindingAdjudicationEntry) bool { return old.FindingID == choice.FindingID })
		if index >= 0 {
			entries[index] = entry
		} else {
			entries = append(entries, entry)
		}
	}
	decision := domain.AdjudicationDecision{CommandID: command.CommandID, ItemID: command.ItemID, ItemVersion: command.ItemVersion}
	var artifact domain.FindingAdjudication
	if prior == nil {
		artifact, err = domain.NewHumanFindingAdjudication(record.RunID, record.Round, binding.run.SpecDigest, record.InstructionDigest, binding.resolvedPolicy.Digest, entries, decision, *item.DecidedAt)
	} else {
		artifact, err = domain.NewHumanSuccessorFindingAdjudication(*prior, entries, decision, *item.DecidedAt)
	}
	if err != nil {
		return nil, err
	}
	if err := runDurableTransitionHook(w.transitionHook, DurableTransitionFindingAdjudication, DurableTransitionBefore); err != nil {
		return nil, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutFindingAdjudication(ctx, artifact) }); err != nil {
		return nil, err
	}
	if err := runDurableTransitionHook(w.transitionHook, DurableTransitionFindingAdjudication, DurableTransitionAfter); err != nil {
		return nil, err
	}
	return &artifact, nil
}
