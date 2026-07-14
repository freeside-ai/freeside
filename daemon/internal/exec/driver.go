package exec

import (
	"context"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// StageDriver runs stages as bounded batch jobs (plan §5.3). Every operation
// is keyed by the daemon-generated invocation id passed to Start, so the
// invocation is reconcilable across daemon restarts and provider crashes.
// Operations on an id never started return ErrUnknownInvocation.
type StageDriver interface {
	// Start commits the invocation intent and begins execution. A second
	// Start with the same id returns ErrDuplicateStart: intent is committed
	// once per id (§5.3), and a restart-after-crash reconciles via
	// Inspect/Collect, never by starting again.
	Start(ctx context.Context, id domain.InvocationID, spec StartSpec) error
	// Inspect reports the invocation's current lifecycle status.
	Inspect(ctx context.Context, id domain.InvocationID) (Status, error)
	// Stream returns a reader over the invocation's transcript so far. The
	// transcript is durably recorded (§5.3 session durability), so the
	// stream is replayable: each call reads from the beginning, and reading
	// concurrently with execution never loses committed output. The caller
	// closes it.
	Stream(ctx context.Context, id domain.InvocationID) (io.ReadCloser, error)
	// Cancel stops a non-terminal invocation and commits a canceled result,
	// so cancellation stays reconcilable like any other outcome. Canceling
	// an invocation that already committed a result is a no-op: the
	// committed result stands (at most one result per id).
	Cancel(ctx context.Context, id domain.InvocationID) error
	// Collect returns the committed terminal result. It is idempotent:
	// repeated calls re-deliver the identical result, and accepting it at
	// most once is the caller's job, not the driver's. Before a result is
	// committed it returns ErrResultNotReady; if the session was lost before
	// any result was committed it returns ErrNoResult.
	Collect(ctx context.Context, id domain.InvocationID) (StageResult, error)
}

// StartSpec is what a driver needs to run one stage attempt. It is
// deliberately minimal for Phase 1: recovery is guaranteed from stage inputs,
// workspace state, and artifacts (§5.3), all digest- or reference-addressed
// here. Real drivers widen this through kind:contract changes when they land,
// not by side channels.
type StartSpec struct {
	RunID   domain.RunID   `json:"run_id"`
	StageID domain.StageID `json:"stage_id"`
	// InputDigest is the content address of the stage's input bundle (spec,
	// prompt, and prior artifacts are bound by digest, §5.9).
	InputDigest domain.Digest `json:"input_digest"`
	// Workspace is an opaque workspace reference; the ward lane defines its
	// shape (§5.7). Drivers pass it through, never interpret it.
	Workspace string `json:"workspace"`
}

// StageResult is the committed terminal outcome of a stage invocation: the
// serialized contract the store persists and the engine accepts (at most
// once, §5.3).
type StageResult struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	// Status is the terminal outcome: completed, failed, or canceled.
	Status Status `json:"status"`
	// HeadSHA is the workspace head the stage left behind, when the stage
	// produced one; empty for stages that move no head.
	HeadSHA string `json:"head_sha"`
	// Artifacts lists the content addresses of the invocation's recorded
	// outputs (transcripts, logs, produced files), §5.15.
	Artifacts []domain.Digest `json:"artifacts"`
	// Summary is the driver's short human-readable outcome description.
	Summary string `json:"summary"`
}

// Validate reports whether the result is well-formed: a result must be
// reconcilable (non-empty invocation id) and terminal. It is the
// deserialization backstop for results reconstructed from the store.
func (r StageResult) Validate() error {
	if r.InvocationID == "" {
		return fmt.Errorf("stage result invocation_id: %w", domain.ErrEmptyID)
	}
	if !r.Status.valid() {
		return fmt.Errorf("stage result status %q: %w", r.Status, ErrInvalidStatus)
	}
	if !r.Status.Terminal() {
		return fmt.Errorf("stage result status %q: %w", r.Status, ErrNonTerminalResult)
	}
	for i, d := range r.Artifacts {
		if d == "" {
			return fmt.Errorf("stage result artifacts[%d]: %w", i, domain.ErrEmptyID)
		}
	}
	return nil
}
