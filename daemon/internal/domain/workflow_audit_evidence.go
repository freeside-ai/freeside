package domain

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const (
	// MaxWorkflowAuditEvidenceBytes bounds the canonical GitHub workflow and
	// repository-settings snapshot retained for one audit digest. The live
	// audit fails closed rather than persisting an unbounded repository body.
	MaxWorkflowAuditEvidenceBytes = 16 << 20

	redactedWorkflowAuditEvidence = "[REDACTED WORKFLOW AUDIT EVIDENCE]"
)

// WorkflowAuditEvidence is the canonical JSON body addressed by a
// WorkflowAuditDigest. Its bytes may contain complete workflow and local
// action files, so ordinary formatting redacts them. JSON marshaling is the
// deliberate review projection; WorkflowAudit itself excludes Evidence from
// its persisted facts body so retention can delete evidence independently of
// the append-only audit ledger.
type WorkflowAuditEvidence struct {
	body string
}

// NewWorkflowAuditEvidence validates and copies one canonical evidence body.
func NewWorkflowAuditEvidence(body []byte) (WorkflowAuditEvidence, error) {
	if len(body) == 0 {
		return WorkflowAuditEvidence{}, fmt.Errorf("workflow audit evidence: %w", ErrWorkflowAuditEvidenceInvalid)
	}
	if len(body) > MaxWorkflowAuditEvidenceBytes {
		return WorkflowAuditEvidence{}, fmt.Errorf(
			"workflow audit evidence size %d exceeds %d: %w",
			len(body), MaxWorkflowAuditEvidenceBytes, ErrWorkflowAuditEvidenceTooLarge,
		)
	}
	var envelope struct {
		Version string `json:"version"`
		Repo    string `json:"repo"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return WorkflowAuditEvidence{}, fmt.Errorf("workflow audit evidence JSON: %w: %w", err, ErrWorkflowAuditEvidenceInvalid)
	}
	if envelope.Version == "" || envelope.Repo == "" {
		return WorkflowAuditEvidence{}, fmt.Errorf("workflow audit evidence envelope: %w", ErrWorkflowAuditEvidenceInvalid)
	}
	return WorkflowAuditEvidence{body: string(body)}, nil
}

// ValidateBinding proves that the retained body is the exact repository
// evidence addressed by digest.
func (e WorkflowAuditEvidence) ValidateBinding(repo string, digest Digest) error {
	if e.body == "" {
		return fmt.Errorf("workflow audit evidence: %w", ErrWorkflowAuditEvidenceInvalid)
	}
	var envelope struct {
		Version string `json:"version"`
		Repo    string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(e.body), &envelope); err != nil {
		return fmt.Errorf("workflow audit evidence JSON: %w: %w", err, ErrWorkflowAuditEvidenceInvalid)
	}
	if envelope.Version == "" || envelope.Repo != repo || e.Digest() != digest {
		return fmt.Errorf("workflow audit evidence for %q: %w", repo, ErrWorkflowAuditEvidenceMismatch)
	}
	return nil
}

// Digest returns the content address of the exact retained JSON bytes.
func (e WorkflowAuditEvidence) Digest() Digest {
	return Digest(contentaddr.Sum([]byte(e.body)))
}

// Bytes returns a caller-owned copy for protected persistence.
func (e WorkflowAuditEvidence) Bytes() []byte {
	return []byte(e.body)
}

// MarshalJSON deliberately reveals the body only when a consumer explicitly
// places this evidence type in the review projection.
func (e WorkflowAuditEvidence) MarshalJSON() ([]byte, error) {
	if e.body == "" {
		return nil, fmt.Errorf("marshal workflow audit evidence: %w", ErrWorkflowAuditEvidenceInvalid)
	}
	return e.Bytes(), nil
}

func (WorkflowAuditEvidence) String() string   { return redactedWorkflowAuditEvidence }
func (WorkflowAuditEvidence) GoString() string { return redactedWorkflowAuditEvidence }

// Format keeps every fmt verb redacted, including %#v and byte-oriented
// verbs that would otherwise expose the unexported body.
func (WorkflowAuditEvidence) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, redactedWorkflowAuditEvidence)
}
