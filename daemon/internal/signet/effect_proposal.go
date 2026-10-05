package signet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// OpenEffectProposalItem opens (or returns) the effect_proposal attention item
// for one stored closure proposal instance and the prospective merge its pull
// request has now. It and OpenEffectProposalNotice are the only paths that
// create a closure's effect_proposal item, and OpenFollowUpFilingItem the only
// one that creates a filing's: generic intake refuses the type
// (ValidateItemIntake), so the trusted context here (a re-gated closure
// instance and a validated merge) is the item's sole authority.
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
	return s.openEffectProposalItem(ctx, instanceID, merge, false)
}

// OpenEffectProposalNotice opens (or returns) the default-policy fallback notice
// for a daemon_fallback closure proposal: the same effect_proposal card, but an
// exceptional interruption whose reason says the pull request is not held, and
// whose approval never authorizes a close (the fallback's resolves flag is
// always false, so ClosureApproval.AuthorizesClose yields false). It refuses a
// propose_site instance, which takes the gate card instead. Idempotency and
// supersession key on the merge exactly as the gate item does. The plan tracks
// the exceptional-interruption rate as a health signal (§3.2), which is what a
// closure-site failure is (plan revision 68, #1487).
func (s *Service) OpenEffectProposalNotice(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
	merge domain.ProspectiveMerge,
) (domain.AttentionItem, error) {
	return s.openEffectProposalItem(ctx, instanceID, merge, true)
}

// openEffectProposalItem is the shared open routine behind the gate item and the
// fallback notice. The notice flag selects the interruption class and reason and
// restricts the origin: the gate card admits any closure instance (a
// daemon_fallback under an on gate is a planned gate too), while the notice is
// only for a daemon_fallback under the default policy.
func (s *Service) openEffectProposalItem(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
	merge domain.ProspectiveMerge,
	notice bool,
) (domain.AttentionItem, error) {
	if err := merge.Validate(); err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open effect proposal item %q: %w", instanceID, err)
	}
	class := domain.InterruptionPlannedGate
	reason := "Decide the proposed effect on the source issue"
	if notice {
		class = domain.InterruptionExceptional
		reason = "The source issue could not be closed automatically; decide the fallback. This notice does not hold the pull request."
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
		// The notice is the daemon_fallback variant; a propose_site instance takes
		// the gate card instead, never a non-holding notice.
		if notice && instance.Proposal.ClosureProposal.Origin != domain.ClosureFlagOriginDaemonFallback {
			return fmt.Errorf("open effect proposal notice %q: not a daemon_fallback proposal: %w",
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
		item, artifact, err := s.newEffectProposalItem(ctx, tx, instance, merge, class, reason)
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
// The interruption class and reason are the caller's: the gate card is a planned
// gate, the fallback notice an exceptional interruption that does not hold.
func (s *Service) newEffectProposalItem(
	ctx context.Context,
	tx *store.WriteTx,
	instance domain.ProposalInstance,
	merge domain.ProspectiveMerge,
	class domain.InterruptionClass,
	reason string,
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
		Reason:            reason,
		RequestedDecision: actions,
		EvidenceSnapshot:  []domain.Artifact{artifact},
		ItemVersion:       1,
		DisplayNames:      names,
		InterruptionClass: class,
		Status:            domain.StatusOpen,
		PRHeadSHA:         merge.CandidateHeadSHA,
		CreatedAt:         &now,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.AttentionItem{}, domain.Artifact{}, err
	}
	return item, artifact, nil
}

// followUpFilingItemReason is fixed daemon text. The finding and adjudicator
// text a filing carries appears only in its proposal's screened title and
// body, never in the item's reason.
const followUpFilingItemReason = "Decide whether to file the follow-up issue"

// OpenFollowUpFilingItem opens (or returns) the effect_proposal attention item
// for one stored follow_up_filing instance. It runs inside the caller's
// transaction, so a producer commits the instance, the item, and their binding
// together with the row that concluded the filing's source.
//
// The instance is read back by ID, which re-runs the registry gate, so the
// item's authority is the stored row and never a caller's struct. A filing
// binds no prospective merge, so nothing supersedes its item and the ID needs
// no per-open suffix: a repeat call returns the item it finds, open or
// decided, and writes nothing. The item offers approve, decline, and snooze;
// a filing cannot be approved with changes.
func OpenFollowUpFilingItem(
	ctx context.Context,
	tx *store.WriteTx,
	instanceID domain.ProposalInstanceID,
	now time.Time,
) (domain.AttentionItem, error) {
	instance, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q: %w", instanceID, err)
	}
	filing := instance.Proposal.FilingProposal
	if instance.Proposal.Kind != domain.EffectFollowUpFiling || filing == nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q: not a filing proposal: %w",
			instanceID, domain.ErrEffectProposalInconsistent)
	}
	itemID := domain.ItemID(string(instance.ID) + "/effect")
	existing, err := tx.GetAttentionItem(ctx, itemID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q: %w", instanceID, err)
	}
	declaration, _, err := tx.ResolveProposalSubject(ctx, filing.SubjectHandle)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q: %w", instanceID, err)
	}
	subject := domain.Subject{
		Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID),
	}
	names, err := tx.DisplayNamesFor(ctx, declaration.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q: %w", instanceID, err)
	}
	artifact, err := instance.EvidenceArtifact()
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q: %w", instanceID, err)
	}
	createdAt := now.UTC()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:                itemID,
		ProjectID:         declaration.ProjectID,
		Subject:           subject,
		Type:              domain.AttentionEffectProposal,
		Priority:          domain.PriorityNormal,
		Reason:            followUpFilingItemReason,
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		ItemVersion:       1,
		DisplayNames:      names,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
		CreatedAt:         &createdAt,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q: %w", instanceID, err)
	}
	if err := tx.PutArtifact(ctx, artifact); err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q artifact: %w", instanceID, err)
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q item: %w", instanceID, err)
	}
	if err := tx.BindProposalItem(ctx, item.ID, instance.ID, instance.Proposal.Digest, nil); err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open follow-up filing item %q bind: %w", instanceID, err)
	}
	return item, nil
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
