package publish

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// sourceIssueCommentAuditor is a WorkflowAuditor over in-memory files: it runs
// the real issue_comment enumeration, so a fixture edit reaches the recheck
// the way an edit on the default branch would.
type sourceIssueCommentAuditor struct {
	workflows map[string]string
	actions   map[string]string
	err       error
	// mutate, when set, alters the audit after it is built.
	mutate func(*domain.WorkflowAudit)
}

func (a sourceIssueCommentAuditor) Audit(_ context.Context, repo, baseRef string) (domain.WorkflowAudit, error) {
	if a.err != nil {
		return domain.WorkflowAudit{}, a.err
	}
	var sources []workflowSource
	for _, p := range slices.Sorted(maps.Keys(a.workflows)) {
		sources = append(sources, workflowSource{Path: p, Content: []byte(a.workflows[p]), Active: true})
	}
	var actionSources []localActionSource
	for _, p := range slices.Sorted(maps.Keys(a.actions)) {
		actionSources = append(actionSources, localActionSource{Path: p, Content: []byte(a.actions[p])})
	}
	listed, err := issueCommentWorkflows(sources, actionSources)
	if err != nil {
		return domain.WorkflowAudit{}, err
	}
	audit := domain.WorkflowAudit{
		Repo: repo, AuditedCommitSHA: "base-sha", AuditedRef: baseRef,
		AuditedAt:             time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC),
		WorkflowAuditDigest:   "sha256:workflow-audit",
		EffectiveTokenPerms:   domain.TokenPermissionsReadOnly,
		IssueCommentWorkflows: listed,
	}
	if a.mutate != nil {
		a.mutate(&audit)
	}
	return audit, nil
}

func approvedIssueCommentProfile(t *testing.T) domain.AutomationTrustProfile {
	t.Helper()
	workflows, actions := issueCommentClosureFixture()
	audit, err := sourceIssueCommentAuditor{workflows: workflows, actions: actions}.Audit(t.Context(), "freeside-ai/demo", "main")
	if err != nil {
		t.Fatalf("fixture audit: %v", err)
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/demo",
		RepositoryID:               123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        audit.WorkflowAuditDigest,
		Review:                     domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config"},
		AllowIssueCommentWorkflows: true,
		IssueCommentWorkflows:      audit.IssueCommentWorkflows,
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	return profile
}

func TestRecheckIssueCommentWorkflows(t *testing.T) {
	t.Parallel()
	const bot = ".github/workflows/bot.yml"
	tests := []struct {
		name       string
		edit       func(workflows, actions map[string]string)
		wantPath   string
		wantChange domain.IssueCommentWorkflowChange
	}{
		{name: "unchanged set", edit: func(map[string]string, map[string]string) {}},
		{name: "added workflow", edit: func(w, _ map[string]string) {
			w[".github/workflows/triage.yml"] = "on: issue_comment\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo t\n"
		}, wantPath: ".github/workflows/triage.yml", wantChange: domain.IssueCommentWorkflowAdded},
		{
			name: "removed workflow", edit: func(w, _ map[string]string) { delete(w, bot) },
			wantPath: bot, wantChange: domain.IssueCommentWorkflowRemoved,
		},
		{name: "listed workflow loses the trigger", edit: func(w, _ map[string]string) {
			w[bot] = "on: issues" + w[bot][len("on: issue_comment"):]
		}, wantPath: bot, wantChange: domain.IssueCommentWorkflowRemoved},
		{name: "unlisted workflow gains the trigger", edit: func(w, _ map[string]string) {
			w[".github/workflows/ci.yml"] = "on: [pull_request, issue_comment]" + w[".github/workflows/ci.yml"][len("on: pull_request"):]
		}, wantPath: ".github/workflows/ci.yml", wantChange: domain.IssueCommentWorkflowAdded},
		{
			name: "listed workflow edited, triggers unchanged", edit: func(w, _ map[string]string) { w[bot] += "# edited\n" },
			wantPath: bot, wantChange: domain.IssueCommentWorkflowChanged,
		},
		{
			name: "local composite action edited", edit: func(_, a map[string]string) { a[".github/actions/c/action.yml"] += "# edited\n" },
			wantPath: bot, wantChange: domain.IssueCommentWorkflowChanged,
		},
		{
			name: "local reusable workflow edited", edit: func(w, _ map[string]string) { w[".github/workflows/shared.yml"] += "# edited\n" },
			wantPath: bot, wantChange: domain.IssueCommentWorkflowChanged,
		},
		{name: "unrelated workflow edited", edit: func(w, _ map[string]string) { w[".github/workflows/ci.yml"] += "# edited\n" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			profile := approvedIssueCommentProfile(t)
			workflows, actions := issueCommentClosureFixture()
			tt.edit(workflows, actions)
			err := RecheckIssueCommentWorkflows(t.Context(), sourceIssueCommentAuditor{workflows: workflows, actions: actions}, profile, "main")
			if tt.wantChange == "" {
				if err != nil {
					t.Fatalf("recheck refused an approved set: %v", err)
				}
				return
			}
			var drift *domain.IssueCommentWorkflowDriftError
			if !errors.As(err, &drift) || !errors.Is(err, domain.ErrIssueCommentWorkflowsNotApproved) {
				t.Fatalf("recheck error = %v, want an issue_comment drift", err)
			}
			if drift.Path != tt.wantPath || drift.Change != tt.wantChange {
				t.Fatalf("drift = %s %s, want %s %s", drift.Path, drift.Change, tt.wantPath, tt.wantChange)
			}
		})
	}
}

// TestRecheckIssueCommentWorkflowsFailsClosed: an audit failure or a profile
// whose digest no longer matches its content is an error, never approval, and
// neither is reported as a set the owner declined.
func TestRecheckIssueCommentWorkflowsFailsClosed(t *testing.T) {
	t.Parallel()
	profile := approvedIssueCommentProfile(t)
	workflows, actions := issueCommentClosureFixture()

	auditErr := errors.New("audit unavailable")
	err := RecheckIssueCommentWorkflows(t.Context(), sourceIssueCommentAuditor{err: auditErr}, profile, "main")
	if !errors.Is(err, auditErr) {
		t.Fatalf("audit failure error = %v, want the audit error", err)
	}

	actions[".github/actions/a/action.yml"] = "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/missing\n"
	err = RecheckIssueCommentWorkflows(t.Context(), sourceIssueCommentAuditor{workflows: workflows, actions: actions}, profile, "main")
	if err == nil || errors.Is(err, domain.ErrIssueCommentWorkflowsNotApproved) {
		t.Fatalf("absent local action error = %v, want an audit error", err)
	}

	// An audit of another repository, or one that fails its own validation,
	// never approves, even when its set matches.
	workflows, actions = issueCommentClosureFixture()
	otherRepo := sourceIssueCommentAuditor{
		workflows: workflows, actions: actions,
		mutate: func(a *domain.WorkflowAudit) { a.Repo = "freeside-ai/other" },
	}
	if err := RecheckIssueCommentWorkflows(t.Context(), otherRepo, profile, "main"); err == nil {
		t.Fatal("audit of another repository approved")
	}
	unsorted := sourceIssueCommentAuditor{
		workflows: workflows, actions: actions,
		mutate: func(a *domain.WorkflowAudit) {
			a.IssueCommentWorkflows = append(a.IssueCommentWorkflows, a.IssueCommentWorkflows[0])
		},
	}
	if err := RecheckIssueCommentWorkflows(t.Context(), unsorted, profile, "main"); !errors.Is(err, domain.ErrIssueCommentWorkflowsInvalid) {
		t.Fatalf("non-canonical audit error = %v, want ErrIssueCommentWorkflowsInvalid", err)
	}

	tampered := profile
	tampered.IssueCommentWorkflows = slices.Clone(profile.IssueCommentWorkflows)
	tampered.IssueCommentWorkflows[0].Digest = "sha256:other"
	workflows, actions = issueCommentClosureFixture()
	err = RecheckIssueCommentWorkflows(t.Context(), sourceIssueCommentAuditor{workflows: workflows, actions: actions}, tampered, "main")
	if !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("tampered profile error = %v, want ErrProfileDigestMismatch", err)
	}
}
