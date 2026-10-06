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
	client        *inference.Client
	store         *store.Store
	artifacts     ArtifactStore
	beginTaskWork func(context.Context, domain.RunID) (context.Context, func(), error)
}

func (a *productionFindingAdjudicator) Adjudicate(
	ctx context.Context, request findingAdjudicationRequest,
) ([]domain.FindingAdjudicationEntry, error) {
	if a == nil || a.client == nil || a.store == nil || a.artifacts == nil {
		return nil, inference.ErrAdjudicationNotAvailable
	}
	if a.beginTaskWork != nil {
		workCtx, finish, err := a.beginTaskWork(ctx, request.RunID)
		if err != nil {
			return nil, err
		}
		defer finish()
		ctx = workCtx
	}
	var (
		run          domain.Run
		policy       domain.ResolvedPolicy
		dispositions []domain.ReviewDispositionRecord
		diffMetrics  *domain.ReviewRoundDiffMetrics
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
		if err != nil {
			return err
		}
		// A round recorded without metrics is a gap, not a failure.
		metrics, err := tx.GetReviewRoundDiffMetrics(ctx, request.RunID, request.Round)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		diffMetrics = &metrics
		return nil
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
	findings, externalFindings, err := adjudicationFindingInputs(request.Findings)
	if err != nil {
		return nil, err
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
			ExternalFindings:  externalFindings,
			PriorDispositions: dispositions,
			PriorEntries:      slices.Clone(request.PriorEntries),
			Dissent:           dissent, Feedback: feedback,
			DiffMetrics: diffMetrics,
		})
}

// adjudicationFindingInputs splits the residue into the two lists the
// adjudicator reads. A reviewer outside Freeside wrote an external finding's
// message, so the adjudicator reads it quoted and cut, in a list of its own,
// and never as a finding (issue #1767 decision 3).
func adjudicationFindingInputs(inputs []findingAdjudicationInput) (
	[]inference.AdjudicationFinding, []inference.ExternalAdjudicationFinding, error,
) {
	findings := make([]inference.AdjudicationFinding, 0, len(inputs))
	var externalFindings []inference.ExternalAdjudicationFinding
	for _, input := range inputs {
		if input.Finding.External != nil {
			quoted, err := quoteExternalFinding(input.Finding)
			if err != nil {
				return nil, nil, err
			}
			externalFindings = append(externalFindings, inference.ExternalAdjudicationFinding{
				FindingID: quoted.FindingID, ReviewerLogin: quoted.ReviewerLogin,
				ThreadID: quoted.ThreadID, HeadSHA: quoted.HeadSHA, Location: quoted.Location,
				QuotedText: quoted.QuotedText, Notice: quoted.Notice,
				RemediationSurface: input.Surface, Compatibility: input.Compatibility,
			})
			continue
		}
		findings = append(findings, inference.AdjudicationFinding{
			Finding: input.Finding, Classification: input.Classification,
			RemediationSurface: input.Surface, Compatibility: input.Compatibility,
		})
	}
	return findings, externalFindings, nil
}
