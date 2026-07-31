package integration_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func integrationWorkflowAuditEvidence(
	t *testing.T, repo, marker string,
) domain.WorkflowAuditEvidence {
	t.Helper()
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/test","repo":"` + repo +
			`","workflows":[{"path":".github/workflows/` + marker + `.yml"}]}`,
	))
	if err != nil {
		t.Fatalf("workflow audit evidence: %v", err)
	}
	return evidence
}
