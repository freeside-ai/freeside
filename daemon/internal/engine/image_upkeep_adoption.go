package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// imagePolicyAdoptionBatchVersion versions the batch-ID preimage.
const imagePolicyAdoptionBatchVersion = "image-policy-adoption/v1"

// imagePolicyAdoptionWithdrawnReason is the daemon's conclusion text for a
// card the repository has moved past.
const imagePolicyAdoptionWithdrawnReason = "Withdrawn: the repository no longer needs this adoption, " +
	"because its tip or admitted image changed."

// imagePolicyAdoptionBatchID is the one batch a refusal's card is admitted
// in, so the card is found from its refusal.
func imagePolicyAdoptionBatchID(refusal domain.Digest) domain.ProposalBatchID {
	sum := sha256.Sum256([]byte(imagePolicyAdoptionBatchVersion + "\x00" + string(refusal)))
	return domain.ProposalBatchID("batch-image-policy-adoption-" + hex.EncodeToString(sum[:]))
}

// proposeAdoption raises the adoption card that answers a policy-class
// refusal (plan §5.7, Project-image upkeep). It re-derives the exact digest
// and hosts from the checkout at the refusal's tip, and raises none when the
// re-derivation finds a clause no adoption answers or the facts do not cover
// every finding: the refusal is then a repair. The card is admitted under
// the refusal-keyed admission key, and only a fresh insert opens its item,
// so a rerun of the same refusal raises no second card.
func (u *ImageUpkeep) proposeAdoption(
	ctx context.Context, dir string, image domain.ProjectImage, refusal domain.ImageUpkeepRefusal,
) error {
	adoptedHosts, err := adoptedRegistryHosts(ctx, u.Store, u.RepositoryID)
	if err != nil {
		return err
	}
	adoption, adoptable, err := projectimage.DeriveUpkeepAdoption(
		ctx, "git", dir, image, refusal.TipSHA, u.Policy, adoptedHosts)
	if err != nil {
		return fmt.Errorf("derive the adoption for refusal %s: %w", refusal.ID, err)
	}
	if !adoptable || !adoptionAnswers(adoption, refusal) {
		u.Logger.Warn("image upkeep: no adoption answers the policy refusal", "refusal", refusal.ID)
		return nil
	}
	now := u.Now().UTC()
	return u.Store.Write(ctx, func(tx *store.WriteTx) error {
		project, found, err := u.adoptionProject(ctx, &tx.ReadTx)
		if err != nil {
			return err
		}
		if !found {
			u.Logger.Warn("image upkeep: no configured project of the repository takes the adoption card",
				"repository_id", u.RepositoryID, "refusal", refusal.ID)
			return nil
		}
		params := domain.ImagePolicyAdoptionParameters{
			ProjectID: project, RefusalRecordID: refusal.ID,
			RecipeDigest: adoption.RecipeDigest, RegistryAuthorities: adoption.Hosts,
		}
		// The facts come from the repository: a tip with no recipe, a host
		// the registry set can't name, or too many hosts is no card to
		// raise, not a fault in this pass.
		if err := params.Validate(); err != nil {
			u.Logger.Warn("image upkeep: the tip's adoption facts are not a card",
				"refusal", refusal.ID, "error", err)
			return nil
		}
		proposal, err := domain.NewImagePolicyAdoptionProposal(params)
		if err != nil {
			return err
		}
		instance, inserted, err := tx.AllocateProposalInstance(ctx, domain.ProposalAdmissionKey{
			Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: refusal.ID,
		}, imagePolicyAdoptionBatchID(refusal.ID), proposal, now)
		if err != nil || !inserted {
			return err
		}
		_, err = signet.OpenImagePolicyAdoptionItem(ctx, tx, u.Blobs, instance.ID, now)
		return err
	})
}

// adoptionAnswers reports whether adopting adoption clears every finding of
// refusal: a changed recipe needs the tip's digest, and an authority finding
// needs at least one host.
func adoptionAnswers(adoption projectimage.UpkeepAdoption, refusal domain.ImageUpkeepRefusal) bool {
	for _, finding := range refusal.Findings {
		switch finding.Clause {
		case domain.RebuildRefusalRecipeChanged:
			if adoption.RecipeDigest == nil {
				return false
			}
		case domain.RebuildRefusalUndeclaredAuthority, domain.RebuildRefusalNoRegistrySet:
			if len(adoption.Hosts) == 0 {
				return false
			}
		default:
			return false
		}
	}
	return len(refusal.Findings) != 0
}

// adoptionProject is the first of Projects registered for the repository.
func (u *ImageUpkeep) adoptionProject(ctx context.Context, tx *store.ReadTx) (domain.ProjectID, bool, error) {
	for _, id := range u.Projects {
		project, err := tx.GetProject(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if project.RepositoryID == u.RepositoryID {
			return id, true, nil
		}
	}
	return "", false, nil
}

// withdrawAdoptions concludes as superseded every open adoption card in
// Projects that does not answer current, the refusal a settled pass reached
// (nil when it reached none), so a card the repository has moved past can't
// be approved. A stale approve is refused at the decision too
// (signet.ErrImagePolicyAdoptionStale); this keeps the queue clear of it.
func (u *ImageUpkeep) withdrawAdoptions(ctx context.Context, current *domain.ImageUpkeepRefusal) error {
	stale, err := u.staleAdoptionItems(ctx, current)
	if err != nil || len(stale) == 0 {
		return err
	}
	now := u.Now().UTC()
	return u.Store.Write(ctx, func(tx *store.WriteTx) error {
		for _, id := range stale {
			item, err := tx.GetAttentionItem(ctx, id)
			if err != nil {
				return err
			}
			// A decision may have landed since the read.
			if item.Status != domain.StatusOpen {
				continue
			}
			item.ItemVersion++
			item, err = item.ConcludeByDaemon(domain.ConcludedBySupersession, now, imagePolicyAdoptionWithdrawnReason, nil)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
}

func (u *ImageUpkeep) staleAdoptionItems(
	ctx context.Context, current *domain.ImageUpkeepRefusal,
) ([]domain.ItemID, error) {
	var stale []domain.ItemID
	err := u.Store.Read(ctx, func(tx *store.ReadTx) error {
		for _, project := range u.Projects {
			ids, err := tx.ProposalItemIDsForSubject(
				ctx, domain.OpaqueSubjectHandle(project), domain.EffectImagePolicyAdoption)
			if err != nil {
				return err
			}
			for _, id := range ids {
				item, err := tx.GetAttentionItem(ctx, id)
				if err != nil {
					return err
				}
				if item.Status != domain.StatusOpen {
					continue
				}
				instance, _, err := tx.ProposalForItem(ctx, id)
				if err != nil {
					return err
				}
				if current != nil && instance.Proposal.AdoptionProposal.RefusalRecordID == current.ID {
					continue
				}
				stale = append(stale, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find stale adoption cards: %w", err)
	}
	return stale, nil
}

// PendingAdoption is the AdoptionPending lookup: the item of refusal's
// adoption card while that card can still resolve the refusal, which is
// while it is open, or once it is approved until upkeep settles past the
// refusal. A declined or withdrawn card resolves nothing, so held work
// takes the blocked outcome.
func (u *ImageUpkeep) PendingAdoption(
	ctx context.Context, refusal domain.ImageUpkeepRefusal,
) (domain.ItemID, bool, error) {
	var (
		itemID  domain.ItemID
		pending bool
	)
	err := u.Store.Read(ctx, func(tx *store.ReadTx) error {
		batch, err := tx.ListProposalBatch(ctx, imagePolicyAdoptionBatchID(refusal.ID))
		if err != nil || len(batch) == 0 {
			return err
		}
		instance := batch[0]
		item, err := tx.GetAttentionItem(ctx, signet.ImagePolicyAdoptionItemID(instance.ID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		decision, err := tx.ProposalDecisionForInstance(ctx, instance.ID)
		if err != nil {
			return err
		}
		approved := decision != nil && decision.Action == domain.ActionApprove
		itemID, pending = item.ID, item.Status == domain.StatusOpen || approved
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return itemID, pending, nil
}
