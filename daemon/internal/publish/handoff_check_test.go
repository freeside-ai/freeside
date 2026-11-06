package publish_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// TestAuditHandoffTipRequiresTheRequestedTarget pins that the handoff audit
// is the auditor's observation of exactly the repository and ref the handoff
// asked for, at a full commit: the store binds the sealed tip to that audit's
// ref, so an audit of any other target is refused before it is recorded.
func TestAuditHandoffTipRequiresTheRequestedTarget(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		mutate  func(*domain.WorkflowAudit)
		wantErr error
	}{
		"the requested target":  {mutate: func(*domain.WorkflowAudit) {}},
		"another ref":           {func(a *domain.WorkflowAudit) { a.AuditedRef = "release" }, publish.ErrUnauthorizedPublication},
		"no ref":                {func(a *domain.WorkflowAudit) { a.AuditedRef = "" }, publish.ErrUnauthorizedPublication},
		"another repository":    {func(a *domain.WorkflowAudit) { a.Repo = "freeside-ai/other-repo" }, publish.ErrUnauthorizedPublication},
		"an abbreviated commit": {func(a *domain.WorkflowAudit) { a.AuditedCommitSHA = "1111" }, publish.ErrUnauthorizedPublication},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			audit := testWorkflowAudit(t)
			audit.AuditedRef, audit.AuditedCommitSHA = "main", testBaseSHA
			test.mutate(&audit)
			p := publish.NewPublisher(testTokenSource(), nil, "", fixedWorkflowAuditor{audit: audit}, nil, nil, nil)
			got, err := p.AuditHandoffTip(t.Context(), testTrustRepo, "main")
			if !errors.Is(err, test.wantErr) || (test.wantErr == nil) != (err == nil) {
				t.Fatalf("AuditHandoffTip error = %v, want %v", err, test.wantErr)
			}
			if err == nil && (got.AuditedRef != "main" || got.AuditedCommitSHA != testBaseSHA) {
				t.Fatalf("AuditHandoffTip = %+v, want main at %s", got, testBaseSHA)
			}
		})
	}
}
