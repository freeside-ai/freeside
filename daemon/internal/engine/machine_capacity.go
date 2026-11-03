package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// WithMachineCapacity bounds ward launches by the instance's memory budget
// (plan §5.4). Each execution admission, review admission, and verification
// job reserves its declared memory plus the runtime's per-VM overhead in the
// transaction that admits it, and a launch that does not fit waits under a
// machine_capacity hold in its task's submission order. The zero bound
// reserves nothing, which is every composition given no budget.
func WithMachineCapacity(bound ward.LaunchBound) Option {
	return func(e *Engine) error {
		if bound.BudgetMiB < 0 {
			return fmt.Errorf("machine capacity: budget %d MiB: %w", bound.BudgetMiB, domain.ErrNegativeSize)
		}
		e.launchBound = bound
		return nil
	}
}

// executionMemory is the reservation an execution admission of run makes,
// read in the transaction that gates it: the writer size the run's policy
// resolves, which is the size the stage driver will declare. It is nil with
// no budget, and for an admission that names no identity, which takes no pool
// slot and starts no ward writer (store.RequireExecutionMemory).
func (e *Engine) executionMemory(
	ctx context.Context, tx *store.ReadTx, run domain.Run, admission domain.ExecutionAdmission,
) (*store.MemoryReservation, error) {
	if !e.launchBound.Budgeted() || admission.AuthIdentityID == nil {
		return nil, nil
	}
	policy, err := runResolvedPolicy(ctx, tx, run)
	if err != nil {
		return nil, fmt.Errorf("writer size policy: %w", err)
	}
	sizes, err := ward.ResolveLaunchSizes(policy.Keys, ward.LaunchBound{})
	if err != nil {
		return nil, err
	}
	reservation, _, err := e.launchBound.LaunchReservation(
		admission.InvocationID, run.ID, domain.ReservationExecution, sizes.Writer)
	if err != nil {
		return nil, err
	}
	return &store.MemoryReservation{
		BudgetMiB: e.launchBound.BudgetMiB, Reservation: reservation, At: time.Now().UTC(),
	}, nil
}

// reviewReservation is the reservation a review admission makes at the ward's
// review size; ok is false with no budget.
func reviewReservation(
	bound ward.LaunchBound, runID domain.RunID, id domain.InvocationID,
) (domain.LaunchReservation, bool, error) {
	return bound.LaunchReservation(id, runID, domain.ReservationReview, ward.DefaultLaunchSize(ward.LaunchReview))
}

// launchMemoryRefusedPrefix names the notice of a launch the budget refuses
// for good (productionQuarantineOccurrenceID).
const launchMemoryRefusedPrefix = "launch-memory-refused-"

// launchMemoryRefusal reports whether err refuses a launch for good under the
// budget: its reservation exceeds the whole budget, or the runtime has no
// recorded overhead to reserve with. Neither is a wait, so neither holds.
func launchMemoryRefusal(err error) bool {
	return errors.Is(err, domain.ErrLaunchNeverFits) || errors.Is(err, domain.ErrRuntimeOverheadUnknown)
}

// launchMemoryRefusalReason is the notice's operator-facing reason, naming the
// numbers or the runtime version the refusal names.
func launchMemoryRefusalReason(err error) string {
	var never *domain.LaunchNeverFitsError
	if errors.As(err, &never) {
		return fmt.Sprintf("This task's launch can never fit the machine's memory budget: "+
			"%d MiB declared plus %d MiB per-VM overhead exceeds the %d MiB budget. "+
			"Raise the budget or lower the policy's size, or stop the task.",
			never.SizeMiB, never.OverheadMiB, never.BudgetMiB)
	}
	var unknown *domain.RuntimeOverheadUnknownError
	if errors.As(err, &unknown) {
		return fmt.Sprintf("The container runtime %q has no recorded per-VM memory overhead, "+
			"so no launch can reserve memory under the budget. A daemon upgrade that records it, "+
			"or a start without a budget, lets the task continue; or stop the task.", unknown.RuntimeVersion)
	}
	return "This task's launch was refused under the machine's memory budget."
}

// noticeLaunchMemoryRefusal records the execution_failure notice for a launch
// the budget refuses for good. The launch stays pending, so a later start
// whose budget or runtime fits it continues the task; the notice converges
// on the open one across passes and offers stop. A machine_capacity hold an
// earlier start recorded no longer describes the run, so it is cleared; it is
// read first because the refusal repeats every pass and a write would move
// the revision each time.
func noticeLaunchMemoryRefusal(
	ctx context.Context, st *store.Store, attention attentionService,
	runID domain.RunID, projectID domain.ProjectID, refusal error,
) error {
	var waited bool
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		hold, present, err := tx.GetRunHold(ctx, runID)
		waited = present && hold.Reason == domain.HoldMachineCapacity
		return err
	}); err != nil {
		return err
	}
	if waited {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.ClearRunHoldCause(ctx, runID, domain.HoldMachineCapacity)
		}); err != nil {
			return err
		}
	}
	return recordProductionQuarantine(
		ctx, st, attention, launchMemoryRefusedPrefix, runID, projectID, launchMemoryRefusalReason(refusal))
}

// recordLaunchOrder keeps a launch's place in the memory order in step with
// why it was refused. A launch refused for memory joins the order, or
// refreshes its wait so the gate keeps counting it (store.LaunchWaitRefresh);
// one refused for anything else, such as a pool slot taken since it joined,
// leaves it, so it holds back no launch on another pool. A refused launch is
// retried every pass and every write advances the revision, so the order is
// written only when it changes or the wait is due a refresh.
func recordLaunchOrder(
	ctx context.Context, st *store.Store, bound ward.LaunchBound,
	runID domain.RunID, invocationID domain.InvocationID, refusal error, now time.Time,
) error {
	if !bound.Budgeted() || refusal == nil {
		return nil
	}
	join := errors.Is(refusal, domain.ErrMachineCapacity)
	var (
		waiting   bool
		refusedAt time.Time
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		refusedAt, waiting, err = tx.LaunchWaitRefusedAt(ctx, invocationID)
		return err
	}); err != nil {
		return fmt.Errorf("launch %q memory order: %w", invocationID, err)
	}
	if join == waiting && (!join || now.Sub(refusedAt) < store.LaunchWaitRefresh) {
		return nil
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if join {
			return tx.RecordLaunchWait(ctx, invocationID, runID, now)
		}
		return tx.LeaveLaunchWait(ctx, invocationID)
	}); err != nil {
		return fmt.Errorf("launch %q memory order: %w", invocationID, err)
	}
	return nil
}
