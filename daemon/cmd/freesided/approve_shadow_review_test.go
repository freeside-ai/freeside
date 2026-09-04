package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestApproveShadowReviewCommandTwoPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	profile := approveShadowReviewProfile(t)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profile, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	configuration := approveShadowReviewDigest("a")
	args := []string{
		profile.Repo, "-db", dbPath,
		"-source", string(domain.ShadowReviewClaudeLocal),
		"-configuration-digest", string(configuration),
	}
	var stdout, stderr bytes.Buffer
	if err := runApproveShadowReviewCommand(ctx, args, &stdout, &stderr); err != nil {
		t.Fatalf("review pass: %v; stderr=%s", err, stderr.String())
	}
	var review operations.ShadowReviewConfigurationApprovalResult
	if err := json.Unmarshal(stdout.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if review.Status != "review_required" || review.ApprovalDigest == "" {
		t.Fatalf("review = %#v", review)
	}
	stdout.Reset()
	stderr.Reset()
	args = append(args, "-approve", string(review.ApprovalDigest))
	if err := runApproveShadowReviewCommand(ctx, args, &stdout, &stderr); err != nil {
		t.Fatalf("approval pass: %v; stderr=%s", err, stderr.String())
	}
	var complete operations.ShadowReviewConfigurationApprovalResult
	if err := json.Unmarshal(stdout.Bytes(), &complete); err != nil {
		t.Fatal(err)
	}
	if complete.Status != "complete" || complete.Approval != review.Approval {
		t.Fatalf("complete = %#v, review = %#v", complete, review)
	}
	st, err := store.OpenReadOnly(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireShadowReviewConfigurationApproved(
			ctx, domain.ShadowReviewClaudeLocal, configuration,
		)
	}); err != nil {
		t.Fatalf("approved command result did not satisfy gate: %v", err)
	}
}

func TestParseApproveShadowReviewConfigRejectsUnreviewableInput(t *testing.T) {
	t.Parallel()
	valid := []string{
		"example/repo", "-db", "freeside.db",
		"-source", string(domain.ShadowReviewClaudeLocal),
		"-configuration-digest", string(approveShadowReviewDigest("a")),
	}
	if _, err := parseApproveShadowReviewConfig(valid, &bytes.Buffer{}); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing repository", nil},
		{"missing db", []string{"example/repo", "-source", "claude_local", "-configuration-digest", string(approveShadowReviewDigest("a"))}},
		{"invalid source", []string{"example/repo", "-db", "db", "-source", "routed", "-configuration-digest", string(approveShadowReviewDigest("a"))}},
		{"invalid configuration digest", []string{"example/repo", "-db", "db", "-source", "claude_local", "-configuration-digest", "sha256:bad"}},
		{"invalid approval digest", append(append([]string(nil), valid...), "-approve", "sha256:bad")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseApproveShadowReviewConfig(tc.args, &bytes.Buffer{}); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func approveShadowReviewProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        approveShadowReviewDigest("1"),
		Review: domain.ReviewSettings{
			Mode:         domain.ReviewFreesideInvoked,
			ConfigDigest: approveShadowReviewDigest("2"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func approveShadowReviewDigest(char string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(char, 64))
}
