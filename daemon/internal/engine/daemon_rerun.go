package engine

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A daemon re-run gives a production run's root implementation invocation
// its next attempt when the daemon itself ended it (issue #3105): a restart
// that lost it, or a graceful shutdown or forced drain that canceled it. The
// attempt is allocated on the same campaign, in the transaction that records
// the terminal, and no execution_failure card is raised. Like the quota park,
// the rule grants nothing: reattemptProductionRunTx allocates the attempt,
// and dispatch re-gates its admission against current policy like any other.
//
// A loss is safe to re-run because the stage driver records it only after
// ward proves every runtime object absent and releases the identity's
// auth-store lease, so no second writer can start beside a live first one.
// Only a loss of an invocation an earlier process started counts as lost to
// a restart. Ward also closes a live handoff as a loss when the writer
// outlives its stop budget, refuses to start, or loses its provider proxy;
// the process that started it sees that loss, and it keeps its card. A
// process that dies after such a live loss but before recording it hands
// the next process an ordinary restart loss, which the limit bounds.
// A canceled root invocation, on a task with no cancellation and an open
// workflow, can only be the daemon's: a task Stop records the task's
// cancellation before it cancels work.
//
// Losses are capped per chain at daemonRerunLimit, read from run lineage
// (daemonRerunLosses), so the count survives restarts with no counter of its
// own. Cancellations never count: routine updates would otherwise exhaust
// the limit and raise cards for healthy work.

const (
	// daemonLossRerunReason is the attempt reason of a re-run after a
	// restart lost the parent's root implementation invocation.
	daemonLossRerunReason = "Lost to a daemon restart; re-run from its base."
	// daemonCancelRerunReason is the attempt reason of a re-run after a
	// daemon shutdown or forced drain canceled the parent's root
	// implementation invocation.
	daemonCancelRerunReason = "Canceled by a daemon shutdown or forced drain; re-run from its base."
	// daemonRerunLimit is how many losses one chain re-runs. The next loss
	// raises the card.
	daemonRerunLimit = 2
)

// daemonRerunCandidate reports whether a terminal is one a daemon re-run
// may answer: a lost or canceled root implementation invocation.
func daemonRerunCandidate(run domain.Run, terminal productionTerminalRecord) bool {
	return terminal.InvocationID == productionInvocationID(run.ID) &&
		(terminal.Status == exec.StatusGone || terminal.Status == exec.StatusCanceled)
}

// daemonRerunReason returns the attempt reason the terminal's re-run
// carries, or "" when the terminal keeps today's failure card. That is: not
// a daemon re-run candidate, no attempt to allocate (a stopped or closed
// task, a run outside a campaign or not its latest attempt), a loss of an
// invocation this process started (startedHere), or a loss past the chain's
// limit. A remediation round or operator-feedback invocation is
// never a candidate: a fresh attempt starts from the run's base and would
// discard the reviewed work, as the quota park decides.
func daemonRerunReason(
	ctx context.Context, tx *store.WriteTx, run domain.Run, terminal productionTerminalRecord, startedHere bool,
) (string, error) {
	if !daemonRerunCandidate(run, terminal) {
		return "", nil
	}
	if terminal.Status == exec.StatusGone && startedHere {
		return "", nil
	}
	if allocatable, err := productionAttemptParkable(ctx, tx, run); err != nil || !allocatable {
		return "", err
	}
	if terminal.Status == exec.StatusCanceled {
		return daemonCancelRerunReason, nil
	}
	losses, err := daemonRerunLosses(ctx, &tx.ReadTx, run)
	if err != nil {
		return "", err
	}
	if losses+1 > daemonRerunLimit {
		return "", nil
	}
	return daemonLossRerunReason, nil
}

// daemonRerunLosses counts the losses already re-run on run's chain: the
// unbroken line of attempts, walking back through ParentRunID, that carry
// either daemon re-run reason. A run carries the loss reason because its
// parent was lost, so each one is a loss. Any other reason ends the chain,
// so an operator retry or a quota park starts a new one.
func daemonRerunLosses(ctx context.Context, tx *store.ReadTx, run domain.Run) (int, error) {
	losses := 0
	for current := run; isDaemonRerunReason(current.AttemptReason); {
		if current.AttemptReason == daemonLossRerunReason {
			losses++
		}
		parent, err := tx.GetRun(ctx, current.ParentRunID)
		if err != nil {
			return 0, fmt.Errorf("daemon re-run chain of %q: %w", run.ID, err)
		}
		current = parent
	}
	return losses, nil
}

func isDaemonRerunReason(reason string) bool {
	return reason == daemonLossRerunReason || reason == daemonCancelRerunReason
}

// rerunProductionAttempt allocates the run's next attempt under reason. It
// runs after the terminal's milestone, which the allocation reads to know
// the parent run is final. It records no hold: the attempt waits only on
// ordinary gates, and the drain gate holds it under update_drain while a
// drain is in force.
func rerunProductionAttempt(ctx context.Context, tx *store.WriteTx, run domain.Run, reason string) error {
	if _, err := reattemptProductionRunTx(ctx, tx, ProductionReattemptSpec{
		ParentRunID: run.ID, Reason: reason,
	}); err != nil {
		return fmt.Errorf("daemon re-run of %q: %w", run.ID, err)
	}
	return nil
}

// transitionHook is the engine's crash-matrix seam for boundaries it owns
// outside a lane, nil when no specification workflow is configured.
func (e *Engine) transitionHook() DurableTransitionHook {
	if e.specification == nil {
		return nil
	}
	return e.specification.transitionHook
}
