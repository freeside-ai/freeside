package engine

import (
	"context"
	"errors"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// recoverScopeConflictTask parks a valid candidate before any verification,
// review, or forge effect. The immutable policy and export remain its authority.
func (w *productionPublicationWorkflow) recoverScopeConflictTask(
	ctx context.Context, task *productionPublicationTask, binding productionBinding,
) (*productionTaskOutcome, error) {
	var claims []domain.AgentClaim
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		claims, err = tx.GetAgentClaims(ctx, task.ProducingInvocationID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		return nil, err
	}
	var claim *domain.AgentClaim
	var source *export.EvidenceEntry
	for _, entry := range task.Replay.Evidence.Entries {
		if entry.Label != export.ScopeConflictEvidenceLabel {
			continue
		}
		if source != nil || entry.Provenance.ProducerInvocationID != string(task.ProducingInvocationID) {
			return nil, domain.ErrParentKeyMismatch
		}
		source = &entry
	}
	for _, c := range claims {
		if c.Label != export.ScopeConflictEvidenceLabel {
			continue
		}
		if claim != nil || c.Provenance.ProducerInvocationID != task.ProducingInvocationID {
			return nil, domain.ErrParentKeyMismatch
		}
		claim = &c
	}
	if claim == nil {
		if source != nil {
			return nil, domain.ErrParentKeyMismatch
		}
		// A replacement candidate cannot erase a recorded obligation or reuse
		// consent bound to another head.
		err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			task.scopeDecision, err = tx.ScopeDecisionForCandidate(ctx, task.RunID, task.HeadSHA)
			return err
		})
		return nil, err
	}
	if source == nil {
		return nil, domain.ErrParentKeyMismatch
	}
	raw, err := readBoundedArtifactBlob(w.artifacts, domain.Digest(source.Digest), int64(domain.MaxScopeConflictBytes))
	if err != nil {
		return nil, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	released, err := domain.DecodeScopeConflict(raw)
	if err != nil {
		return nil, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	canonical, err := domain.EncodeScopeConflict(released)
	if err != nil {
		return nil, err
	}
	if domain.Digest(contentaddr.Sum(canonical)) != claim.Digest {
		return nil, domain.ErrParentKeyMismatch
	}
	body, err := readBoundedArtifactBlob(w.artifacts, claim.Digest, int64(domain.MaxScopeConflictBytes))
	if err != nil {
		return nil, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	conflict, err := domain.DecodeScopeConflict(body)
	if err != nil {
		return nil, errors.Join(domain.ErrParentKeyMismatch, err)
	}
	facts := &domain.ScopeConflictFacts{Paths: conflict.Paths, DeclaredPaths: domain.CanonicalDeclaredPaths(binding.resolvedPolicy), HeadSHA: task.HeadSHA}
	if err := facts.Validate(); err != nil {
		return nil, err
	}
	var item domain.AttentionItem
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, productionQuestionItemID(task.ProducingInvocationID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		now := w.attentionCreatedAt()
		err = w.store.Write(ctx, func(tx *store.WriteTx) error {
			subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &task.RunID}
			names, err := tx.DisplayNamesFor(ctx, task.ProjectID, subject)
			if err != nil {
				return err
			}
			questionClaim := *claim
			questionClaim.Label = domain.AgentQuestionClaimLabel
			kind := domain.BlockedKindScopeExpansion
			item, err = domain.NewAttentionItem(domain.AttentionItemInput{
				ID: productionQuestionItemID(task.ProducingInvocationID), ProjectID: task.ProjectID, Subject: subject,
				Type: domain.AttentionAgentQuestion, Priority: domain.PriorityNormal, Reason: conflict.Decision.Question,
				RequestedDecision: []domain.Action{domain.ActionAnswerWithoutRetry, domain.ActionStop},
				AgentClaims:       []domain.AgentClaim{questionClaim}, PRHeadSHA: task.HeadSHA,
				AgentQuestion: &domain.AgentQuestionFacts{
					Stage: domain.StageNameImplementation, InvocationID: task.ProducingInvocationID,
					Kind: &kind, Decisions: []domain.Decision{conflict.Decision}, ScopeConflict: facts,
				},
				ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen, CreatedAt: &now, DisplayNames: names,
			}, nil)
			if err != nil {
				return err
			}
			if err := recordRunHold(ctx, tx, task.RunID, task.PublicationID, domain.HoldScopeConflict, now); err != nil {
				return err
			}
			return tx.PutAttentionItem(ctx, item)
		})
	}
	if err != nil {
		return nil, err
	}
	if item.AgentQuestion == nil || !reflect.DeepEqual(item.AgentQuestion.ScopeConflict, facts) ||
		!reflect.DeepEqual(item.AgentQuestion.Decisions, []domain.Decision{conflict.Decision}) {
		return nil, domain.ErrParentKeyMismatch
	}
	if item.Status == domain.StatusOpen {
		w.deferHeldTask(*task)
		return &productionTaskOutcome{}, nil
	}
	var command domain.Command
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		command, err = tx.ScopeConflictCommand(ctx, item)
		if err != nil {
			return err
		}
		if command.Action == domain.ActionAnswerWithoutRetry {
			task.scopeDecision, err = tx.ScopeDecisionForCandidate(ctx, task.RunID, task.HeadSHA)
		}
		return err
	}); err != nil {
		return nil, err
	}
	if command.Action == domain.ActionStop {
		cause := domain.HoldScopeConflict
		if err := w.appendPublicationMilestone(ctx, *task, domain.MilestonePublicationBlocked, &cause); err != nil {
			return nil, err
		}
		accepted, err := w.recordCompletedTerminalAtBoundary(ctx, binding.run, *task, signet.PublicationReevaluationBlocked)
		if err != nil {
			return nil, err
		}
		if err := w.finishTask(ctx, *task); err != nil {
			return nil, err
		}
		return &productionTaskOutcome{completed: true, accepted: accepted, blocked: true}, nil
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ClearRunHoldCause(ctx, task.RunID, domain.HoldScopeConflict)
	}); err != nil {
		return nil, err
	}
	return nil, nil
}
