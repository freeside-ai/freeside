package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func validShadowReviewConfigurationApprovalInput() domain.ShadowReviewConfigurationApprovalInput {
	return domain.ShadowReviewConfigurationApprovalInput{
		Repo:                "freeside-ai/freeside",
		RepositoryID:        123456789,
		Source:              domain.ShadowReviewClaudeLocal,
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
	}
}

func TestShadowReviewConfigurationApprovalAuthenticatesEveryFact(t *testing.T) {
	approval, err := domain.NewShadowReviewConfigurationApproval(
		validShadowReviewConfigurationApprovalInput(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.ShadowReviewConfigurationApproval)
	}{
		{"repo", func(a *domain.ShadowReviewConfigurationApproval) { a.Repo = "other/repo" }},
		{"repository id", func(a *domain.ShadowReviewConfigurationApproval) { a.RepositoryID++ }},
		{"source", func(a *domain.ShadowReviewConfigurationApproval) { a.Source = "other" }},
		{"configuration digest", func(a *domain.ShadowReviewConfigurationApproval) {
			a.ConfigurationDigest = domain.Digest("sha256:" + strings.Repeat("d", 64))
		}},
		{"approval digest", func(a *domain.ShadowReviewConfigurationApproval) {
			a.ApprovalDigest = domain.Digest("sha256:" + strings.Repeat("e", 64))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := approval
			tc.mutate(&mutated)
			if err := mutated.Validate(); !errors.Is(
				err, domain.ErrShadowApprovalDigestMismatch,
			) && tc.name != "source" {
				t.Fatalf("mutated approval error = %v, want digest mismatch", err)
			} else if tc.name == "source" && !errors.Is(err, domain.ErrInvalidShadowReviewSource) {
				t.Fatalf("mutated source error = %v, want invalid source", err)
			}
		})
	}
}

func TestShadowReviewConfigurationApprovalRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   error
		mutate func(*domain.ShadowReviewConfigurationApprovalInput)
	}{
		{"empty repo", domain.ErrEmptyField, func(in *domain.ShadowReviewConfigurationApprovalInput) { in.Repo = "" }},
		{"noncanonical repo", domain.ErrRepositoryIdentityMismatch, func(in *domain.ShadowReviewConfigurationApprovalInput) { in.Repo = "owner/repo/extra" }},
		{"zero repository id", domain.ErrNonPositive, func(in *domain.ShadowReviewConfigurationApprovalInput) { in.RepositoryID = 0 }},
		{"invalid source", domain.ErrInvalidShadowReviewSource, func(in *domain.ShadowReviewConfigurationApprovalInput) { in.Source = "routed" }},
		{"invalid configuration digest", domain.ErrInvalidReviewCompletionEvidence, func(in *domain.ShadowReviewConfigurationApprovalInput) {
			in.ConfigurationDigest = "sha256:not-canonical"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validShadowReviewConfigurationApprovalInput()
			tc.mutate(&in)
			if _, err := domain.NewShadowReviewConfigurationApproval(in); !errors.Is(err, tc.want) {
				t.Fatalf("invalid approval error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestShadowReviewConfigurationApprovalGolden(t *testing.T) {
	approval, err := domain.NewShadowReviewConfigurationApproval(
		validShadowReviewConfigurationApprovalInput(),
	)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(approval, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "shadow_review_configuration_approval", append(body, '\n'))
}
