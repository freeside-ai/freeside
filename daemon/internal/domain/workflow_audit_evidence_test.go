package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestWorkflowAuditEvidenceBindingAndRedaction(t *testing.T) {
	const (
		repo   = "freeside-ai/evidence-repo"
		needle = "workflow-secret-shaped-needle"
	)
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + repo +
			`","workflows":[{"content":"` + needle + `"}]}`,
	))
	if err != nil {
		t.Fatalf("NewWorkflowAuditEvidence: %v", err)
	}
	if err := evidence.ValidateBinding(repo, evidence.Digest()); err != nil {
		t.Fatalf("ValidateBinding: %v", err)
	}
	if err := evidence.ValidateBinding("freeside-ai/other", evidence.Digest()); !errors.Is(err, domain.ErrWorkflowAuditEvidenceMismatch) {
		t.Fatalf("repo mismatch error = %v", err)
	}
	if err := evidence.ValidateBinding(repo, "sha256:other"); !errors.Is(err, domain.ErrWorkflowAuditEvidenceMismatch) {
		t.Fatalf("digest mismatch error = %v", err)
	}

	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		if got := fmt.Sprintf(format, evidence); strings.Contains(got, needle) {
			t.Fatalf("format %s leaked evidence: %s", format, got)
		}
	}

	audit := domain.WorkflowAudit{
		Repo: repo, AuditedCommitSHA: "cafebabe",
		AuditedAt:           time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		WorkflowAuditDigest: evidence.Digest(),
		Evidence:            &evidence,
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	body, err := json.Marshal(audit)
	if err != nil {
		t.Fatalf("marshal audit: %v", err)
	}
	if strings.Contains(string(body), needle) {
		t.Fatalf("ordinary audit serialization leaked evidence: %s", body)
	}
	projected, err := json.Marshal(struct {
		Evidence domain.WorkflowAuditEvidence `json:"evidence"`
	}{Evidence: evidence})
	if err != nil {
		t.Fatalf("marshal review projection: %v", err)
	}
	if !strings.Contains(string(projected), needle) {
		t.Fatalf("explicit review projection omitted evidence: %s", projected)
	}
}

func TestWorkflowAuditEvidenceRejectsInvalidAndOversizedBodies(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty":           nil,
		"invalid JSON":    []byte(`{"version":`),
		"missing version": []byte(`{"repo":"freeside-ai/evidence-repo"}`),
		"missing repo":    []byte(`{"version":"v2"}`),
		"oversized":       []byte(`{"version":"v2","repo":"repo","padding":"` + strings.Repeat("x", domain.MaxWorkflowAuditEvidenceBytes) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := domain.NewWorkflowAuditEvidence(body)
			if err == nil {
				t.Fatal("NewWorkflowAuditEvidence succeeded")
			}
			if name == "oversized" && !errors.Is(err, domain.ErrWorkflowAuditEvidenceTooLarge) {
				t.Fatalf("error = %v, want ErrWorkflowAuditEvidenceTooLarge", err)
			}
		})
	}
}
