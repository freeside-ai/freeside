package domain

import (
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ManualSubmission is private intake bookkeeping. Its namespaced identity
// distinguishes deliberate submissions; the digest binds the original request.
// Task identity is held by the corresponding task_intake_keys row.
type ManualSubmission struct {
	Identity            string
	ProjectID           ProjectID
	SourceArtifactID    ArtifactID
	SourceDigest        Digest
	RequestDigest       Digest
	ImplementationRunID RunID
}

func (s ManualSubmission) Validate() error {
	if (!strings.HasPrefix(s.Identity, "client:") && !strings.HasPrefix(s.Identity, "cli:")) ||
		strings.HasSuffix(s.Identity, ":") || s.ProjectID == "" || s.SourceArtifactID == "" || s.ImplementationRunID == "" {
		return fmt.Errorf("manual submission: %w", ErrEmptyID)
	}
	if !contentaddr.Valid(string(s.SourceDigest)) || !contentaddr.Valid(string(s.RequestDigest)) {
		return fmt.Errorf("manual submission: %w", ErrInvalidDigest)
	}
	return nil
}
