package signet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// OpenEffectProposalItem opens (or returns) the effect_proposal attention item
// for one stored closure proposal instance and the prospective merge its pull
// request has now. It is the only path that creates an effect_proposal item:
// generic intake refuses the type (ValidateItemIntake), so the trusted context
// here (a re-gated closure instance and a validated merge) is the item's sole
// authority.
//
// The call is idempotent for the same instance and merge: a repeat returns the
// existing open item and writes nothing. A different merge (a new candidate
// head, a base advance, or a changed publication identity) supersedes the open
// item and opens a fresh one bound to the new merge; that is how #1419 reports a
// merge change. A decision already recorded against the superseded item stays in
// the ledger, and ClosureApproval.AuthorizesClose stops honoring it once the
// merge differs.
func (s *Service) OpenEffectProposalItem(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
	merge domain.ProspectiveMerge,
) (domain.AttentionItem, error) {
	if err := merge.Validate(); err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open effect proposal item %q: %w", instanceID, err)
	}
	var result domain.AttentionItem
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		// GetProposalInstance re-runs the closed effect-registry gate against
		// current rows, so a source that is no longer closable admits no item.
		instance, err := tx.GetProposalInstance(ctx, instanceID)
		if err != nil {
			return fmt.Errorf("open effect proposal item %q: %w", instanceID, err)
		}
		if instance.Proposal.Kind != domain.EffectSourceIssueClosure || instance.Proposal.ClosureProposal == nil {
			return fmt.Errorf("open effect proposal item %q: not a closure proposal: %w",
				instanceID, domain.ErrEffectProposalInconsistent)
		}
		openItem, openMerge, found, err := tx.OpenEffectItemForInstance(ctx, instanceID)
		if err != nil {
			return fmt.Errorf("open effect proposal item %q: %w", instanceID, err)
		}
		if found {
			if openMerge != nil && *openMerge == merge {
				result = openItem
				return nil
			}
			superseded := openItem
			superseded.ItemVersion++
			superseded.Status = domain.StatusSuperseded
			if err := tx.PutAttentionItem(ctx, superseded); err != nil {
				return fmt.Errorf("open effect proposal item %q supersede: %w", instanceID, err)
			}
		}
		item, artifact, err := s.newEffectProposalItem(ctx, tx, instance, merge)
		if err != nil {
			return fmt.Errorf("open effect proposal item %q: %w", instanceID, err)
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return fmt.Errorf("open effect proposal item %q artifact: %w", instanceID, err)
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return fmt.Errorf("open effect proposal item %q item: %w", instanceID, err)
		}
		if err := tx.BindProposalItem(ctx, item.ID, instance.ID, instance.Proposal.Digest, &merge); err != nil {
			return fmt.Errorf("open effect proposal item %q bind: %w", instanceID, err)
		}
		result = item
		return nil
	})
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return result, nil
}

// newEffectProposalItem builds the open closure item and its evidence carrier.
// The item's PRHeadSHA is the candidate head, so the command-binding check and
// the approval binding judge the same head. approve_with_changes is offered only
// when the proposal can be revised to resolve, which a daemon_fallback proposal
// cannot (SourceIssueClosureParameters.Validate forbids a resolving fallback).
func (s *Service) newEffectProposalItem(
	ctx context.Context,
	tx *store.WriteTx,
	instance domain.ProposalInstance,
	merge domain.ProspectiveMerge,
) (domain.AttentionItem, domain.Artifact, error) {
	closure := instance.Proposal.ClosureProposal
	declaration, _, err := tx.ResolveProposalSubject(ctx, closure.SubjectHandle)
	if err != nil {
		return domain.AttentionItem{}, domain.Artifact{}, err
	}
	subject := domain.Subject{
		Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID),
	}
	names, err := tx.DisplayNamesFor(ctx, declaration.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, domain.Artifact{}, err
	}
	artifact, err := instance.EvidenceArtifact()
	if err != nil {
		return domain.AttentionItem{}, domain.Artifact{}, err
	}
	actions := []domain.Action{
		domain.ActionApprove, domain.ActionApproveWithChanges,
		domain.ActionDecline, domain.ActionSnooze,
	}
	if closure.Origin == domain.ClosureFlagOriginDaemonFallback {
		actions = []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze}
	}
	suffix, err := randomItemSuffix()
	if err != nil {
		return domain.AttentionItem{}, domain.Artifact{}, err
	}
	now := s.now().UTC()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:                domain.ItemID(string(instance.ID) + "/effect/" + suffix),
		ProjectID:         declaration.ProjectID,
		Subject:           subject,
		Type:              domain.AttentionEffectProposal,
		Priority:          domain.PriorityNormal,
		Reason:            "Decide the proposed effect on the source issue",
		RequestedDecision: actions,
		EvidenceSnapshot:  []domain.Artifact{artifact},
		ItemVersion:       1,
		DisplayNames:      names,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
		PRHeadSHA:         merge.CandidateHeadSHA,
		CreatedAt:         &now,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.AttentionItem{}, domain.Artifact{}, err
	}
	return item, artifact, nil
}

// randomItemSuffix produces a unique per-open item-id suffix. Each open for a
// changed merge is a distinct item, so the id cannot be derived from the merge
// (a reverted head would then collide with the superseded item that kept its
// id). Idempotency for an unchanged merge is decided by the open-item lookup,
// not by the id.
func randomItemSuffix() (string, error) {
	var body [8]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", fmt.Errorf("generate effect proposal item suffix: %w", err)
	}
	return hex.EncodeToString(body[:]), nil
}
