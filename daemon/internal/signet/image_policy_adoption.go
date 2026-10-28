package signet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrImagePolicyAdoptionStale rejects approving an adoption card whose
// refusal no longer describes the repository: its tip is no longer the
// observed default-branch tip, or its image is no longer the admitted one.
// The transaction rolls back and records nothing; the next upkeep pass
// withdraws the card.
var ErrImagePolicyAdoptionStale = errors.New("image policy adoption card no longer matches the repository")

// imagePolicyAdoptionItemReason is fixed daemon text. The digest and hosts
// the card adopts are its facts (ImagePolicyAdoptionFacts), so the reason
// carries no repository-authored text.
const imagePolicyAdoptionItemReason = "Decide whether to adopt the image policy this repository's tip needs"

// OpenImagePolicyAdoptionItem opens (or returns) the effect_proposal item for
// one stored image_policy_adoption card (plan §5.7, Project-image upkeep). It
// runs inside the caller's transaction, so the upkeep job commits the card,
// its item, and their binding together.
//
// The card is read back by ID, which re-gates it against its project and
// refusal record, so the item's authority is the stored row. The item's
// subject is the card's batch, which the store's adoption write requires of
// the deciding item. A card binds no merge, so nothing supersedes its item by
// merge and a repeat call returns the item it finds, open or decided. The
// item offers approve, decline, and snooze. blobs receives the proposal's
// evidence body; a nil writer fails closed.
func OpenImagePolicyAdoptionItem(
	ctx context.Context,
	tx *store.WriteTx,
	blobs EvidenceBlobWriter,
	instanceID domain.ProposalInstanceID,
	now time.Time,
) (domain.AttentionItem, error) {
	card, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q: %w", instanceID, err)
	}
	params := card.Proposal.AdoptionProposal
	if card.Proposal.Kind != domain.EffectImagePolicyAdoption || params == nil {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q: not an adoption card: %w",
			instanceID, domain.ErrEffectProposalInconsistent)
	}
	itemID := ImagePolicyAdoptionItemID(card.ID)
	existing, err := tx.GetAttentionItem(ctx, itemID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q: %w", instanceID, err)
	}
	subject := domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(card.ProposalBatchID)}
	names, err := tx.DisplayNamesFor(ctx, params.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q: %w", instanceID, err)
	}
	artifact, err := PutProposalEvidence(ctx, tx, blobs, card)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q artifact: %w", instanceID, err)
	}
	createdAt := now.UTC()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:                itemID,
		ProjectID:         params.ProjectID,
		Subject:           subject,
		Type:              domain.AttentionEffectProposal,
		Priority:          domain.PriorityNormal,
		Reason:            imagePolicyAdoptionItemReason,
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		ItemVersion:       1,
		DisplayNames:      names,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
		CreatedAt:         &createdAt,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q: %w", instanceID, err)
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q item: %w", instanceID, err)
	}
	if err := tx.BindProposalItem(ctx, item.ID, card.ID, card.Proposal.Digest, nil); err != nil {
		return domain.AttentionItem{}, fmt.Errorf("open image policy adoption item %q bind: %w", instanceID, err)
	}
	return item, nil
}

// ImagePolicyAdoptionItemID is the one item an adoption card opens.
func ImagePolicyAdoptionItemID(instanceID domain.ProposalInstanceID) domain.ItemID {
	return domain.ItemID(string(instanceID) + "/effect")
}

// recordImagePolicyAdoption applies an approved adoption card inside the
// accepting transaction: the card must still describe the repository, and
// the store then records the adoption, re-gating the card and the approve
// decision this transaction just recorded. The effect is a local record, so
// no outbox carries it; the upkeep wake after commit builds under it.
func recordImagePolicyAdoption(
	ctx context.Context, tx *store.WriteTx, instanceID domain.ProposalInstanceID, now time.Time,
) error {
	card, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if err := gateImagePolicyAdoptionCurrent(ctx, tx, card); err != nil {
		return err
	}
	_, err = tx.RecordImagePolicyAdoption(ctx, instanceID, now)
	return err
}

// gateImagePolicyAdoptionCurrent refuses a card whose refusal no longer
// matches the repository (plan §5.8: a stale card records nothing). The
// refusal must have been measured from the admitted image, at the observed
// default-branch tip when one is observed.
func gateImagePolicyAdoptionCurrent(ctx context.Context, tx *store.WriteTx, card domain.ProposalInstance) error {
	refusal, err := tx.GetImageUpkeepRefusal(ctx, card.Proposal.AdoptionProposal.RefusalRecordID)
	if err != nil {
		return err
	}
	pointer, _, found, err := tx.GetAdmittedImagePointer(ctx, refusal.RepositoryID, refusal.Harness)
	if err != nil {
		return err
	}
	if !found || pointer.ImageID != refusal.MeasuredFromImageID {
		return fmt.Errorf("refusal %s was measured from an image that is no longer admitted: %w",
			refusal.ID, ErrImagePolicyAdoptionStale)
	}
	observation, found, err := tx.GetRepositoryTipObservation(ctx, refusal.RepositoryID)
	if err != nil {
		return err
	}
	if found && observation.TipSHA != refusal.TipSHA {
		return fmt.Errorf("refusal %s is for tip %s, and the observed tip is %s: %w",
			refusal.ID, refusal.TipSHA, observation.TipSHA, ErrImagePolicyAdoptionStale)
	}
	return nil
}
