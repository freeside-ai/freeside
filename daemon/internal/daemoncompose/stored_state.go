package daemoncompose

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ConvergeStoredState runs the startup steps that read and write only the
// store, in the order Start runs them. Start and freesided check-database
// both call it, so the check runs the code a start runs rather than a copy
// of it. workflowEnabled says whether an execution driver is configured; the
// LaunchAgent daemon runs without one. A failure names its step.
func ConvergeStoredState(
	ctx context.Context, st *store.Store, workflowEnabled bool, now func() time.Time, logger *slog.Logger,
) error {
	if err := convergeExecutionConfiguration(ctx, st, workflowEnabled, now); err != nil {
		return &StoredStateError{Step: "report execution configuration", Err: err}
	}
	if err := concludeInvocationStallNotices(ctx, st); err != nil {
		return &StoredStateError{Step: "conclude stale invocation stall notices", Err: err}
	}
	// A completion recorded before migration 0066 gains its
	// work_unit_completed milestone once (#1134); a store already carrying
	// the milestones is a no-op.
	if err := reconcileWorkUnitCompletionMilestones(ctx, st, logger); err != nil {
		return &StoredStateError{Step: "reconcile work-unit completion milestones", Err: err, bare: true}
	}
	return nil
}

// StoredStateError is a failed ConvergeStoredState step. Its text is the one
// Start has always returned; Step lets a caller name the step without
// printing Err, whose text can quote stored values.
type StoredStateError struct {
	Step string
	Err  error
	// bare keeps Err's text unprefixed, as Start has always returned the
	// completion reconcile's error.
	bare bool
}

func (e *StoredStateError) Error() string {
	if e.bare {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %v", e.Step, e.Err)
}

func (e *StoredStateError) Unwrap() error { return e.Err }
