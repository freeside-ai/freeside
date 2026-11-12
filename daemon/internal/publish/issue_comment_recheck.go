package publish

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// RecheckIssueCommentWorkflows takes a fresh workflow audit of the profile's
// repository at baseRef and reports whether its issue_comment workflows are
// exactly the set the profile approved (plan §5.5). It is the precondition an
// operation that posts as the Freeside App checks before posting: the
// specification comment (#1437), the issue_comment effect kind (#3147), and
// dispatch without a card (#3150). It records nothing.
//
// The caller passes the repository's active profile (the store's latest
// approved one) and its default branch: Validate proves the profile's content
// matches its digest, not that the owner activated it, and the plan requires
// the recheck at the current default-branch tip.
//
// The profile is re-validated first, because a decoded profile's approval is a
// stored authority bit whose digest must still match its content. An audit or
// validation failure returns an error, never approval; a set that differs
// returns an error matching domain.ErrIssueCommentWorkflowsNotApproved.
func RecheckIssueCommentWorkflows(ctx context.Context, auditor WorkflowAuditor, profile domain.AutomationTrustProfile, baseRef string) error {
	if err := profile.Validate(); err != nil {
		return fmt.Errorf("recheck issue_comment workflows: %w", err)
	}
	audit, err := auditor.Audit(ctx, profile.Repo, baseRef)
	if err != nil {
		return fmt.Errorf("recheck issue_comment workflows: %w", err)
	}
	if err := audit.Validate(); err != nil {
		return fmt.Errorf("recheck issue_comment workflows: %w", err)
	}
	if audit.Repo != profile.Repo {
		return fmt.Errorf("recheck issue_comment workflows: audit covers %s, not %s", audit.Repo, profile.Repo)
	}
	return domain.EvaluateIssueCommentWorkflowDrift(profile, audit.IssueCommentWorkflows)
}
