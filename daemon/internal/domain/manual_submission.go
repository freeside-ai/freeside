package domain

import (
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ManualSubmission is private intake bookkeeping. Its namespaced identity
// distinguishes deliberate submissions; the digest binds the original request.
// Task identity is held by the corresponding task_intake_keys row.
//
// TaskLines are the per-role agent choices the request carried, in canonical
// order. A cli: record is what admission holds a cli_submit line to (plan
// §5.4). A request with none, or one recorded before the field existed, holds
// nil.
type ManualSubmission struct {
	Identity            string
	ProjectID           ProjectID
	SourceArtifactID    ArtifactID
	SourceDigest        Digest
	RequestDigest       Digest
	ImplementationRunID RunID
	TaskLines           []TaskLineChoice
}

// SameRequest reports whether two records are one request, so that either
// retries the other. RequestDigest covers the lines, so they are not compared
// again: a record from before the field stores none for a request that carried
// some, and a retry of that request still replays it.
func (s ManualSubmission) SameRequest(other ManualSubmission) bool {
	return s.Identity == other.Identity && s.ProjectID == other.ProjectID &&
		s.SourceArtifactID == other.SourceArtifactID && s.SourceDigest == other.SourceDigest &&
		s.RequestDigest == other.RequestDigest && s.ImplementationRunID == other.ImplementationRunID
}

func (s ManualSubmission) Validate() error {
	if (!strings.HasPrefix(s.Identity, "client:") && !strings.HasPrefix(s.Identity, "cli:") &&
		!strings.HasPrefix(s.Identity, "daemon:")) ||
		strings.HasSuffix(s.Identity, ":") || s.ProjectID == "" || s.SourceArtifactID == "" || s.ImplementationRunID == "" {
		return fmt.Errorf("manual submission: %w", ErrEmptyID)
	}
	if !contentaddr.Valid(string(s.SourceDigest)) || !contentaddr.Valid(string(s.RequestDigest)) {
		return fmt.Errorf("manual submission: %w", ErrInvalidDigest)
	}
	if err := ValidateRecordedTaskLines(s.TaskLines); err != nil {
		return fmt.Errorf("manual submission: %w", err)
	}
	return nil
}
