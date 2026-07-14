package exec

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ReviewSource requests and reconciles external reviews (plan §5.3).
// Invocation-id-first like StageDriver: every operation is keyed by the
// daemon-generated id passed to RequestReview, and operations on an id never
// requested return ErrUnknownInvocation.
type ReviewSource interface {
	// RequestReview commits the review intent for the given head and begins
	// the review. A second request with the same id returns
	// ErrDuplicateStart (one committed intent per id, §5.3).
	RequestReview(ctx context.Context, id domain.InvocationID, req ReviewRequest) error
	// Inspect reports the review invocation's current lifecycle status.
	Inspect(ctx context.Context, id domain.InvocationID) (Status, error)
	// Poll returns the committed review result. It is idempotent: repeated
	// polls re-deliver the identical result, and accepting it at most once
	// is the caller's job. Before a result is committed it returns
	// ErrResultNotReady; if the review ended without committing one it
	// returns ErrNoResult.
	Poll(ctx context.Context, id domain.InvocationID) (ReviewResult, error)
	// Verify checks the committed result's freshness: it fails with
	// ErrStaleHead when the head the review actually ran against differs
	// from expectedHead (a review of a superseded head must never gate the
	// current one, §5.3).
	Verify(ctx context.Context, id domain.InvocationID, expectedHead string) error
}

// ReviewRequest is what a review source needs to review one head.
type ReviewRequest struct {
	RunID domain.RunID `json:"run_id"`
	// HeadSHA is the head the review is requested for.
	HeadSHA string `json:"head_sha"`
}

// ReviewResult is the committed outcome of a review invocation: the
// serialized contract the store persists and the engine accepts (at most
// once, §5.3). A clean pass is an empty Findings list; there is no separate
// verdict field to drift from it.
type ReviewResult struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	// HeadSHA is the head the review actually ran against; Verify compares
	// it to the caller's expected head.
	HeadSHA  string           `json:"head_sha"`
	Findings []domain.Finding `json:"findings"`
}

// Validate reports whether the result is well-formed: reconcilable
// (non-empty invocation id), head-bound (a review unbindable to a head can
// never pass Verify), and carrying only well-formed findings. It is the
// deserialization backstop for results reconstructed from the store.
func (r ReviewResult) Validate() error {
	if r.InvocationID == "" {
		return fmt.Errorf("review result invocation_id: %w", domain.ErrEmptyID)
	}
	if r.HeadSHA == "" {
		return fmt.Errorf("review result head_sha: %w", domain.ErrEmptyField)
	}
	for i, f := range r.Findings {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("review result findings[%d]: %w", i, err)
		}
	}
	return nil
}
