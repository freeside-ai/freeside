package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ProposalAdmission is the complete trusted context for one occurrence. The
// parameters carry no target identity or authority; the store resolves their
// opaque WorkUnitID handle to daemon-owned declaration and policy state.
type ProposalAdmission struct {
	ProjectID       domain.ProjectID
	ProposalBatchID domain.ProposalBatchID
	AdmissionKey    domain.ProposalAdmissionKey
	Kind            domain.EffectKind
	Parameters      any
	Priority        domain.Priority
	// RequestedDecision, when set, is the exact offered-action set for the
	// created item; empty defaults to the full run_proposal set. Label intake
	// omits start_with_changes because a label proposal's subject is fixed to
	// the occurrence's own issue, so revising the subject is not a label-intake
	// flow and offering it would strand the occurrence (#659, decision note
	// Decision 4). Signet policy still requires the set be a non-empty subset of
	// the type's allowed actions.
	RequestedDecision []domain.Action
}

// ProposalAdmissionResult is the atomically committed occurrence and its
// digest-bound run_proposal attention item.
type ProposalAdmissionResult struct {
	Instance domain.ProposalInstance
	Item     domain.AttentionItem
}

// AdmitProposal validates through the closed registry, allocates the effect
// identity under the occurrence key, and creates its item in one transaction.
// A retry after an acknowledged or lost response returns the same committed
// identity and consumes no second client-visible revision.
func (e *Engine) AdmitProposal(
	ctx context.Context,
	request ProposalAdmission,
) (ProposalAdmissionResult, error) {
	return e.admitProposalAt(ctx, request, time.Now().UTC())
}

func (e *Engine) admitProposalAt(
	ctx context.Context,
	request ProposalAdmission,
	createdAt time.Time,
) (ProposalAdmissionResult, error) {
	if e == nil || e.store == nil {
		return ProposalAdmissionResult{}, errors.New("admit proposal: nil engine store")
	}
	if request.ProjectID == "" || request.ProposalBatchID == "" {
		return ProposalAdmissionResult{}, fmt.Errorf("admit proposal project or batch: %w", domain.ErrEmptyID)
	}
	if request.Priority == "" {
		request.Priority = domain.PriorityNormal
	}
	var result ProposalAdmissionResult
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		parameters, ok := request.Parameters.(domain.RunProposalParameters)
		if !ok {
			return domain.ErrEffectProposalInconsistent
		}
		declaration, policy, err := tx.ResolveProposalSubject(ctx, parameters.SubjectHandle)
		if err != nil {
			return fmt.Errorf("resolve proposal subject: %w", err)
		}
		if declaration.ProjectID != request.ProjectID {
			return fmt.Errorf("proposal subject project %q differs from request %q: %w",
				declaration.ProjectID, request.ProjectID, domain.ErrParentKeyMismatch)
		}
		if err := domain.GateRunProposalScope(parameters.Scope, declaration); err != nil {
			return fmt.Errorf("proposal scope differs from durable declaration: %w", err)
		}
		proposal, err := domain.NewEffectProposal(request.Kind, parameters, policy)
		if err != nil {
			return fmt.Errorf("construct proposal: %w", err)
		}
		instance, inserted, err := tx.AllocateProposalInstance(
			ctx, request.AdmissionKey, request.ProposalBatchID, proposal,
			createdAt,
		)
		if err != nil {
			return err
		}
		result.Instance = instance
		if !inserted {
			itemID := domain.ItemID(instance.ID)
			boundInstance, boundProposal, err := tx.ProposalForItem(ctx, itemID)
			if err != nil {
				return fmt.Errorf("proposal instance %q has no valid attention binding: %w", instance.ID, err)
			}
			if boundInstance.ID != instance.ID || boundProposal.Digest != instance.Proposal.Digest {
				return fmt.Errorf("proposal instance %q retry binding: %w", instance.ID, store.ErrImmutableConflict)
			}
			item, err := tx.GetAttentionItem(ctx, itemID)
			if err != nil {
				return err
			}
			if err := validateProposalItem(instance, item); err != nil {
				return err
			}
			result.Item = item
			return errReplay
		}
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return fmt.Errorf("persist proposal artifact: %w", err)
		}
		item, err := newProposalItem(request.ProjectID, instance, artifact, request.Priority, request.RequestedDecision)
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return fmt.Errorf("persist proposal item: %w", err)
		}
		if err := tx.BindProposalItem(ctx, item.ID, instance.ID, proposal.Digest); err != nil {
			return fmt.Errorf("bind proposal item: %w", err)
		}
		result.Item = item
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return ProposalAdmissionResult{}, fmt.Errorf("admit proposal: %w", err)
	}
	return result, nil
}

func newProposalItem(
	projectID domain.ProjectID,
	instance domain.ProposalInstance,
	artifact domain.Artifact,
	priority domain.Priority,
	requestedDecision []domain.Action,
) (domain.AttentionItem, error) {
	if instance.Proposal.Kind != domain.EffectRunProposal || instance.Proposal.RunProposal == nil {
		return domain.AttentionItem{}, domain.ErrEffectProposalInconsistent
	}
	// The default full set; a caller may narrow it (label intake drops
	// start_with_changes). NewAttentionItem and signet policy re-gate the set.
	if len(requestedDecision) == 0 {
		requestedDecision = []domain.Action{
			domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
		}
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID(instance.ID), ProjectID: projectID,
		Subject: domain.Subject{
			Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID),
		},
		Type: domain.AttentionRunProposal, Priority: priority,
		Reason:            "Start the daemon-enumerated work subject",
		RequestedDecision: slices.Clone(requestedDecision),
		EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		CreatedAt: &instance.CreatedAt,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
}

func validateProposalItem(instance domain.ProposalInstance, item domain.AttentionItem) error {
	if item.ID != domain.ItemID(instance.ID) || item.Type != domain.AttentionRunProposal ||
		item.Subject.Type != domain.SubjectProposalBatch ||
		item.Subject.ID != domain.SubjectID(instance.ProposalBatchID) ||
		len(item.ArtifactDigests) != 1 || item.ArtifactDigests[0] != instance.Proposal.Digest {
		return fmt.Errorf("proposal instance %q disagrees with attention item: %w", instance.ID, store.ErrImmutableConflict)
	}
	return nil
}
