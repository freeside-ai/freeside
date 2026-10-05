package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

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

// ListExternalFindings returns the external findings stored for one run,
// earliest first by the forge's timestamp and then by ID. Like GetFinding it
// is a history read and grants nothing: each finding still has to pass
// GetAdmittedExternalFinding before it may drive a round. Every finding row
// of the run is reconstructed and validated before the filter, so a damaged
// row fails the read instead of dropping out of it.
func (tx *ReadTx) ListExternalFindings(ctx context.Context, runID domain.RunID) ([]domain.Finding, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT id, body FROM findings WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list external findings %q: %w", runID, err)
	}
	var out []domain.Finding
	for rows.Next() {
		var (
			id   string
			body []byte
		)
		if err := rows.Scan(&id, &body); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list external findings %q: %w", runID, err)
		}
		finding, err := decode[domain.Finding](body)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list external findings %q row %q: %w", runID, id, err)
		}
		if finding.ID != domain.FindingID(id) || finding.RunID != runID {
			_ = rows.Close()
			return nil, fmt.Errorf("list external findings %q row %q: %w", runID, id, errRowInconsistent)
		}
		if finding.External != nil {
			out = append(out, finding)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list external findings %q: %w", runID, err)
	}
	slices.SortStableFunc(out, func(a, b domain.Finding) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return out, nil
}

// ExternalReviewCycleFindings returns the external findings one
// external_review cycle answers: the run's external findings on the cycle's
// head whose reviewer the profile the authority names admits, earliest first.
// The authority is read through its gate, and the named profile is used, not
// the active one, so the set reads the same after the owner edits the
// allowlist. The finding the authority names is always in it.
func (tx *ReadTx) ExternalReviewCycleFindings(
	ctx context.Context, runID domain.RunID, publication domain.InvocationID,
) ([]domain.Finding, error) {
	successor, err := tx.GetPublicationSuccessor(ctx, runID, publication)
	if err != nil {
		return nil, fmt.Errorf("external review cycle findings %q: %w", publication, err)
	}
	if successor.EffectiveOrigin() != domain.PublicationSuccessorExternalReview || successor.Reentry == nil {
		return nil, fmt.Errorf("external review cycle findings %q: not an external review cycle: %w",
			publication, domain.ErrParentKeyMismatch)
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, successor.PredecessorItemID)
	if err != nil || ready.RunID != runID {
		return nil, fmt.Errorf("external review cycle findings %q: %w",
			publication, errors.Join(err, domain.ErrParentKeyMismatch))
	}
	profile, err := tx.GetTrustProfile(ctx, successor.AdmittingProfileDigest)
	if err != nil {
		return nil, fmt.Errorf("external review cycle findings %q: %w",
			publication, errors.Join(err, domain.ErrExternalReviewNotAdmitted))
	}
	all, err := tx.ListExternalFindings(ctx, runID)
	if err != nil {
		return nil, err
	}
	var (
		out   []domain.Finding
		named bool
	)
	for _, finding := range all {
		if finding.External.HeadSHA != successor.Reentry.HeadSHA ||
			externalFindingAdmittedBy(finding, profile, ready) != nil {
			continue
		}
		named = named || finding.ID == successor.ExternalFindingID
		out = append(out, finding)
	}
	if !named {
		return nil, fmt.Errorf("external review cycle findings %q: triggering finding %q is not among them: %w",
			publication, successor.ExternalFindingID, domain.ErrParentKeyMismatch)
	}
	return out, nil
}
