package operations_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestShadowReviewConfigurationApprovalTwoPassAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openShadowConfigurationOperationStore(t)
	profile := shadowConfigurationOperationProfile(t, "example/repo", 44)
	seedShadowConfigurationOperationProfile(t, st, profile)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	approver := operations.ShadowReviewConfigurationApprover{
		Store: st, Now: func() time.Time { return now },
	}
	req := operations.ShadowReviewConfigurationApprovalRequest{
		Repository: profile.Repo, Source: domain.ShadowReviewClaudeLocal,
		ConfigurationDigest: shadowConfigurationOperationDigest("a"),
	}
	review, err := approver.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if review.Status != "review_required" ||
		review.Approval.RepositoryID != profile.RepositoryID ||
		review.ApprovalDigest != review.Approval.ApprovalDigest {
		t.Fatalf("review result = %#v", review)
	}
	assertShadowConfigurationOperationGolden(
		t, "shadow-review-configuration-review-required", review,
	)
	req.ApprovalDigest = review.ApprovalDigest
	complete, err := approver.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if complete.Status != "complete" || complete.Approval != review.Approval {
		t.Fatalf("complete result = %#v, review = %#v", complete, review)
	}
	assertShadowConfigurationOperationGolden(
		t, "shadow-review-configuration-complete", complete,
	)
	// Exact command replay is semantically inert and remains complete.
	if replay, err := approver.Run(ctx, req); err != nil || replay != complete {
		t.Fatalf("replay = %#v, err=%v; want %#v", replay, err, complete)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireShadowReviewConfigurationApproved(
			ctx, req.Source, req.ConfigurationDigest,
		)
	}); err != nil {
		t.Fatalf("activated approval gate: %v", err)
	}
}

func TestShadowReviewConfigurationApprovalRejectsChangedProposal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openShadowConfigurationOperationStore(t)
	profile := shadowConfigurationOperationProfile(t, "example/repo", 44)
	seedShadowConfigurationOperationProfile(t, st, profile)
	approver := operations.ShadowReviewConfigurationApprover{Store: st, Now: time.Now}
	req := operations.ShadowReviewConfigurationApprovalRequest{
		Repository: profile.Repo, Source: domain.ShadowReviewClaudeLocal,
		ConfigurationDigest: shadowConfigurationOperationDigest("a"),
	}
	review, err := approver.Run(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// The active profile now names a different canonical repository identity.
	rotated := shadowConfigurationOperationProfile(t, profile.Repo, 45)
	seedShadowConfigurationOperationProfile(t, st, rotated)
	req.ApprovalDigest = review.ApprovalDigest
	if _, err := approver.Run(ctx, req); err == nil ||
		!strings.Contains(err.Error(), "does not match proposed review") {
		t.Fatalf("stale approval error = %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		inspection, err := tx.InspectCurrentShadowReviewConfigurationApproval(
			ctx, req.Repository, req.Source,
		)
		if err == nil && !errors.Is(inspection.ReconstructionError, store.ErrNotFound) {
			t.Fatalf("stale proposal activated: %#v", inspection)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestShadowReviewConfigurationApprovalRequiresCurrentProfile(t *testing.T) {
	t.Parallel()
	st := openShadowConfigurationOperationStore(t)
	_, err := (operations.ShadowReviewConfigurationApprover{
		Store: st, Now: time.Now,
	}).Run(context.Background(), operations.ShadowReviewConfigurationApprovalRequest{
		Repository: "example/missing", Source: domain.ShadowReviewClaudeLocal,
		ConfigurationDigest: shadowConfigurationOperationDigest("a"),
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing profile error = %v, want not found", err)
	}
}

func assertShadowConfigurationOperationGolden(
	t *testing.T, name string, value any,
) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, name, append(body, '\n'))
}

func openShadowConfigurationOperationStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(
		context.Background(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedShadowConfigurationOperationProfile(
	t *testing.T, st *store.Store, profile domain.AutomationTrustProfile,
) {
	t.Helper()
	if err := st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(context.Background(), profile, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

func shadowConfigurationOperationProfile(
	t *testing.T, repo string, repositoryID int64,
) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: repositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        shadowConfigurationOperationDigest("1"),
		Review: domain.ReviewSettings{
			Mode:         domain.ReviewFreesideInvoked,
			ConfigDigest: shadowConfigurationOperationDigest("2"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func shadowConfigurationOperationDigest(char string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(char, 64))
}
