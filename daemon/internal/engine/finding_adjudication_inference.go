package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// productionFindingAdjudicator adapts the engine's already-derived residue to
// the daemon-side inference boundary. It re-loads every immutable body from
// the request's version binding and never receives implementer reasoning.
type productionFindingAdjudicator struct {
	client    *inference.Client
	store     *store.Store
	artifacts ArtifactStore
}

func (a *productionFindingAdjudicator) Adjudicate(
	ctx context.Context, request findingAdjudicationRequest,
) ([]domain.FindingAdjudicationEntry, error) {
	if a == nil || a.client == nil || a.store == nil || a.artifacts == nil {
		return nil, inference.ErrAdjudicationNotAvailable
	}
	var (
		run          domain.Run
		policy       domain.ResolvedPolicy
		dispositions []domain.ReviewDispositionRecord
	)
	if err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, request.RunID)
		if err != nil {
			return err
		}
		policy, err = tx.GetResolvedPolicy(ctx, request.RunID)
		if err != nil {
			return err
		}
		dispositions, err = tx.ListFindingDispositions(ctx, request.RunID)
		return err
	}); err != nil {
		return nil, fmt.Errorf("load finding-adjudicator bindings: %w", err)
	}
	if run.SpecDigest != request.ApprovedSpecDigest ||
		policy.Digest != request.ResolvedPolicyDigest {
		return nil, errors.Join(inference.ErrAdjudicationNotAvailable, domain.ErrParentKeyMismatch)
	}
	specification, err := loadFakePublicationBlob(a.artifacts, request.ApprovedSpecDigest)
	if err != nil {
		return nil, fmt.Errorf("load approved specification for adjudication: %w", err)
	}
	instructions, err := loadFakePublicationBlob(a.artifacts, request.InstructionSnapshotDigest)
	if err != nil {
		return nil, fmt.Errorf("load instruction snapshot for adjudication: %w", err)
	}
	findings := make([]inference.AdjudicationFinding, 0, len(request.Findings))
	for _, input := range request.Findings {
		findings = append(findings, inference.AdjudicationFinding{
			Finding: input.Finding, Classification: input.Classification,
			RemediationSurface: input.Surface, Compatibility: input.Compatibility,
		})
	}
	var dissent *inference.AdjudicationDissent
	if request.Dissent != nil {
		dissent = &inference.AdjudicationDissent{
			Kind:       string(request.Dissent.Kind),
			FindingIDs: append([]domain.FindingID(nil), request.Dissent.FindingIDs...),
			Evidence:   request.Dissent.Evidence,
		}
	}
	var feedback *inference.AdjudicationFeedback
	if request.Feedback != nil {
		attachments := make([]inference.AdjudicationAttachment, 0, len(request.Feedback.Attachments))
		for _, attachment := range request.Feedback.Attachments {
			attachments = append(attachments, inference.AdjudicationAttachment{
				Digest: attachment.Digest, Content: attachment.Content,
			})
		}
		feedback = &inference.AdjudicationFeedback{
			InvocationID: request.Feedback.InvocationID, ConversationID: request.Feedback.ConversationID,
			ThroughSequence: request.Feedback.ThroughSequence, PrefixDigest: request.Feedback.PrefixDigest,
			ConversationPrefix: json.RawMessage(append([]byte(nil), request.Feedback.ConversationPrefix...)),
			Attachments:        attachments,
		}
	}
	return a.client.AdjudicateFindings(ctx, string(run.ProjectID), string(request.RunID),
		inference.FindingAdjudicationInput{
			RunID: request.RunID, Round: request.Round,
			ApprovedSpecDigest: request.ApprovedSpecDigest, ApprovedSpecification: string(specification),
			InstructionSnapshotDigest: request.InstructionSnapshotDigest,
			InstructionSnapshot:       string(instructions), ResolvedPolicyDigest: request.ResolvedPolicyDigest,
			DeclaredPaths: append([]string(nil), request.DeclaredPaths...), Findings: findings,
			PriorDispositions: dispositions,
			PriorEntries:      slices.Clone(request.PriorEntries),
			Dissent:           dissent, Feedback: feedback,
		})
}
