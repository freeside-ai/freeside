package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ShadowReviewConfigurationApprovalRequest identifies one exact proposal.
// ApprovalDigest is empty for the review pass and must exactly match the
// returned digest for the applying pass. RepositoryID is never caller input:
// each pass resolves it from the current trust profile.
type ShadowReviewConfigurationApprovalRequest struct {
	Repository          string
	Source              domain.ShadowReviewSource
	ConfigurationDigest domain.Digest
	ApprovalDigest      domain.Digest
}

// ShadowReviewConfigurationApprovalResult is the complete owner review on
// both passes. Status is review_required until the exact proposal is active.
type ShadowReviewConfigurationApprovalResult struct {
	Status         string                                   `json:"status"`
	ApprovalDigest domain.Digest                            `json:"approval_digest"`
	Approval       domain.ShadowReviewConfigurationApproval `json:"approval"`
}

// ShadowReviewConfigurationApprover owns the two-pass operator transaction.
type ShadowReviewConfigurationApprover struct {
	Store *store.Store
	Now   func() time.Time
}

// Run returns the current profile-bound proposal on the first pass. On the
// applying pass it re-resolves that profile inside the write transaction,
// rejects proposal drift, and only then records and activates the authority.
func (a ShadowReviewConfigurationApprover) Run(
	ctx context.Context,
	req ShadowReviewConfigurationApprovalRequest,
) (ShadowReviewConfigurationApprovalResult, error) {
	if a.Store == nil || a.Now == nil {
		return ShadowReviewConfigurationApprovalResult{},
			errors.New("approve shadow review configuration: nil dependency")
	}
	if req.Repository == "" {
		return ShadowReviewConfigurationApprovalResult{},
			errors.New("approve shadow review configuration: repository is required")
	}
	propose := func(tx *store.ReadTx) (domain.ShadowReviewConfigurationApproval, error) {
		profile, err := tx.LatestTrustProfile(ctx, req.Repository)
		if err != nil {
			return domain.ShadowReviewConfigurationApproval{},
				fmt.Errorf("resolve current trust profile: %w", err)
		}
		if profile.Repo != req.Repository {
			return domain.ShadowReviewConfigurationApproval{},
				fmt.Errorf("current trust profile repository %q differs: %w",
					profile.Repo, domain.ErrRepositoryIdentityMismatch)
		}
		approval, err := domain.NewShadowReviewConfigurationApproval(
			domain.ShadowReviewConfigurationApprovalInput{
				Repo: profile.Repo, RepositoryID: profile.RepositoryID,
				Source: req.Source, ConfigurationDigest: req.ConfigurationDigest,
			},
		)
		if err != nil {
			return domain.ShadowReviewConfigurationApproval{},
				fmt.Errorf("construct proposal: %w", err)
		}
		return approval, nil
	}
	var proposal domain.ShadowReviewConfigurationApproval
	if err := a.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		proposal, err = propose(tx)
		return err
	}); err != nil {
		return ShadowReviewConfigurationApprovalResult{},
			fmt.Errorf("approve shadow review configuration: %w", err)
	}
	result := ShadowReviewConfigurationApprovalResult{
		Status: "review_required", ApprovalDigest: proposal.ApprovalDigest,
		Approval: proposal,
	}
	if req.ApprovalDigest == "" {
		return result, nil
	}
	if req.ApprovalDigest != proposal.ApprovalDigest {
		return ShadowReviewConfigurationApprovalResult{}, fmt.Errorf(
			"approve shadow review configuration: approval digest %s does not match proposed review %s",
			req.ApprovalDigest, proposal.ApprovalDigest,
		)
	}
	if err := a.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		currentProposal, err := propose(&tx.ReadTx)
		if err != nil {
			return err
		}
		if currentProposal != proposal || currentProposal.ApprovalDigest != req.ApprovalDigest {
			return fmt.Errorf(
				"proposal changed before activation (reviewed %s, current %s): %w",
				proposal.ApprovalDigest, currentProposal.ApprovalDigest,
				domain.ErrRepositoryIdentityMismatch,
			)
		}
		inspection, err := tx.InspectCurrentShadowReviewConfigurationApproval(
			ctx, proposal.Repo, proposal.Source,
		)
		if err != nil {
			return err
		}
		if inspection.ReconstructionError == nil && inspection.Approval == proposal {
			return nil
		}
		now := a.Now().UTC()
		if err := tx.RecordInactiveShadowReviewConfigurationApproval(ctx, proposal, now); err != nil {
			return err
		}
		return tx.ActivateShadowReviewConfigurationApproval(
			ctx, proposal.Repo, proposal.Source, proposal.ApprovalDigest, now,
		)
	}); err != nil {
		return ShadowReviewConfigurationApprovalResult{},
			fmt.Errorf("approve shadow review configuration: %w", err)
	}
	result.Status = "complete"
	return result, nil
}
