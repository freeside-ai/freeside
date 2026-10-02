package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PutExternalFinding persists a finding left on a published pull request by a
// reviewer outside Freeside (plan §5.19). It is the only write door for that
// record: PutFinding refuses one, so it can never be written as part of a
// review record or a shadow review record, and this door refuses an ID that
// either already links. Storing a finding grants nothing; nothing stored says
// its reviewer is admitted. A replayed write converges on byte-identical
// content.
func (tx *WriteTx) PutExternalFinding(ctx context.Context, finding domain.Finding) error {
	if finding.External == nil {
		return fmt.Errorf("put external finding %q: no external provenance: %w",
			finding.ID, domain.ErrExternalFindingInconsistent)
	}
	if err := tx.ensureFindingNotRoutedLinked(ctx, finding.ID); err != nil {
		return fmt.Errorf("put external finding %q routed link: %w", finding.ID, err)
	}
	if err := tx.ensureFindingNotShadowLinked(ctx, finding.ID); err != nil {
		return fmt.Errorf("put external finding %q shadow link: %w", finding.ID, err)
	}
	return tx.putFinding(ctx, finding)
}

// GetAdmittedExternalFinding is the one finding read that grants authority:
// it returns an external finding only while the trust profile active for the
// run's repository admits its reviewer to drive a round. The stored finding
// carries no admission to trust, so the check runs against current state on
// every read and fails closed with ErrExternalReviewNotAdmitted when the
// repository has no active profile, the reviewer is not listed, or the forge,
// account ID, or login differs from the listed entry. It answers who may
// drive a round, not whether this finding may: it does not compare the
// finding's head with the published one, which the re-entry gate does.
// GetFinding stays ungated: reading a finding as history never depends on the
// allowlist.
func (tx *ReadTx) GetAdmittedExternalFinding(ctx context.Context, id domain.FindingID) (domain.Finding, error) {
	finding, err := tx.GetFinding(ctx, id)
	if err != nil {
		return domain.Finding{}, err
	}
	if finding.External == nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: no external provenance: %w",
			id, domain.ErrExternalFindingInconsistent)
	}
	itemID, err := tx.PublishedProductionReadyItemID(ctx, finding.RunID)
	if err != nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w", id, err)
	}
	binding, err := tx.GetReadyItemPRBinding(ctx, itemID)
	if err != nil || binding.RunID != finding.RunID {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w",
			id, errors.Join(err, domain.ErrParentKeyMismatch))
	}
	profile, err := tx.LatestTrustProfile(ctx, binding.Repo)
	if errors.Is(err, ErrNotFound) {
		err = errors.Join(err, domain.ErrExternalReviewNotAdmitted)
	}
	if err != nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w", id, err)
	}
	if err := externalFindingAdmittedBy(finding, profile, binding); err != nil {
		return domain.Finding{}, fmt.Errorf("get admitted external finding %q: %w", id, err)
	}
	return finding, nil
}

// externalFindingAdmittedBy re-runs the allowlist check for one external
// finding against one validated profile. The profile must be for the bound
// repository by its immutable ID, not only its name, so a profile recorded
// for an earlier repository of the same owner/name admits nobody.
func externalFindingAdmittedBy(
	finding domain.Finding, profile domain.AutomationTrustProfile, binding domain.ReadyItemPRBinding,
) error {
	p := finding.External
	if p == nil {
		return domain.ErrExternalFindingInconsistent
	}
	if profile.Repo != binding.Repo || profile.RepositoryID != binding.RepositoryID {
		return fmt.Errorf("trust profile %s is not for repository %s (%d): %w",
			profile.ProfileDigest, binding.Repo, binding.RepositoryID, domain.ErrExternalReviewNotAdmitted)
	}
	authority, ok := profile.ExternalReviewAuthorityFor(p.Forge, p.ReviewerAccountID, p.ReviewerLogin)
	if !ok || authority != domain.ExternalReviewDriveRound {
		return fmt.Errorf("reviewer %s:%d %q under trust profile %s: %w",
			p.Forge, p.ReviewerAccountID, p.ReviewerLogin, profile.ProfileDigest, domain.ErrExternalReviewNotAdmitted)
	}
	return nil
}
