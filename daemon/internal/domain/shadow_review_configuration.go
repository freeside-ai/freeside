package domain

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const shadowReviewConfigurationApprovalEncodingVersion = "freeside-shadow-review-configuration-approval/v1"

// ShadowReviewConfigurationApproval is the independently owner-approved
// authority for one exact shadow-review configuration. It is deliberately
// separate from AutomationTrustProfile: routed review keeps the profile's one
// Review.ConfigDigest, while this record can rotate without changing v6.
type ShadowReviewConfigurationApproval struct {
	Repo                string             `json:"repo"`
	RepositoryID        int64              `json:"repository_id"`
	Source              ShadowReviewSource `json:"source"`
	ConfigurationDigest Digest             `json:"configuration_digest"`
	ApprovalDigest      Digest             `json:"approval_digest"`
}

// ShadowReviewConfigurationApprovalInput omits ApprovalDigest so callers
// cannot claim approval for content that does not resolve to that address.
type ShadowReviewConfigurationApprovalInput struct {
	Repo                string
	RepositoryID        int64
	Source              ShadowReviewSource
	ConfigurationDigest Digest
}

// NewShadowReviewConfigurationApproval constructs self-authenticating owner
// authority for the exact canonical repository identity, source, and config.
func NewShadowReviewConfigurationApproval(
	in ShadowReviewConfigurationApprovalInput,
) (ShadowReviewConfigurationApproval, error) {
	approval := ShadowReviewConfigurationApproval{
		Repo:                in.Repo,
		RepositoryID:        in.RepositoryID,
		Source:              in.Source,
		ConfigurationDigest: in.ConfigurationDigest,
	}
	digest, err := approval.ComputeDigest()
	if err != nil {
		return ShadowReviewConfigurationApproval{}, err
	}
	approval.ApprovalDigest = digest
	if err := approval.Validate(); err != nil {
		return ShadowReviewConfigurationApproval{}, err
	}
	return approval, nil
}

type canonicalShadowReviewConfigurationApproval struct {
	Version             string             `json:"version"`
	Repo                string             `json:"repo"`
	RepositoryID        int64              `json:"repository_id"`
	Source              ShadowReviewSource `json:"source"`
	ConfigurationDigest Digest             `json:"configuration_digest"`
}

// ComputeDigest returns the versioned content address of every approved fact.
func (a ShadowReviewConfigurationApproval) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(canonicalShadowReviewConfigurationApproval{
		Version:             shadowReviewConfigurationApprovalEncodingVersion,
		Repo:                a.Repo,
		RepositoryID:        a.RepositoryID,
		Source:              a.Source,
		ConfigurationDigest: a.ConfigurationDigest,
	})
	if err != nil {
		return "", fmt.Errorf("shadow review configuration approval digest: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate re-authenticates decoded approval content and rejects repository
// names that are not already in exact owner/name form.
func (a ShadowReviewConfigurationApproval) Validate() error {
	owner, name, hasSlash := strings.Cut(a.Repo, "/")
	switch {
	case a.Repo == "":
		return fmt.Errorf("shadow review configuration approval repo: %w", ErrEmptyField)
	case !hasSlash || owner == "" || name == "" || strings.Contains(name, "/") ||
		strings.TrimSpace(a.Repo) != a.Repo:
		return fmt.Errorf("shadow review configuration approval repo %q: %w",
			a.Repo, ErrRepositoryIdentityMismatch)
	case a.RepositoryID <= 0:
		return fmt.Errorf("shadow review configuration approval repository_id %d: %w",
			a.RepositoryID, ErrNonPositive)
	case !a.Source.valid():
		return fmt.Errorf("shadow review configuration approval source %q: %w",
			a.Source, ErrInvalidShadowReviewSource)
	case !contentaddr.Valid(string(a.ConfigurationDigest)):
		return fmt.Errorf("shadow review configuration approval configuration_digest %q: %w",
			a.ConfigurationDigest, ErrInvalidReviewCompletionEvidence)
	case !contentaddr.Valid(string(a.ApprovalDigest)):
		return fmt.Errorf("shadow review configuration approval approval_digest %q: %w",
			a.ApprovalDigest, ErrShadowApprovalDigestMismatch)
	}
	computed, err := a.ComputeDigest()
	if err != nil {
		return err
	}
	if a.ApprovalDigest != computed {
		return fmt.Errorf(
			"shadow review configuration approval digest %q, content resolves to %q: %w",
			a.ApprovalDigest, computed,
			ErrShadowApprovalDigestMismatch,
		)
	}
	return nil
}
