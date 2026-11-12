package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// WithMachineCapacity bounds ward launches by the instance's memory budget
// (plan §5.4). Each execution admission, review admission, and verification
// job reserves its declared memory plus the runtime's per-VM overhead in the
// transaction that admits it, and a launch that does not fit waits under a
// machine_capacity hold in its task's submission order. bound is the bound the
// instance started with; a budget the operator records later
// (freesided set-memory-budget) replaces its BudgetMiB from the next pass
// (launchBoundAt). An unbudgeted bound reserves nothing until then, so it
// carries ward.RuntimeNone for a composition that starts no VM.
func WithMachineCapacity(bound ward.LaunchBound) Option {
	return func(e *Engine) error {
		if bound.BudgetMiB < 0 {
			return fmt.Errorf("machine capacity: budget %d MiB: %w", bound.BudgetMiB, domain.ErrNegativeSize)
		}
		e.launchBound = bound
		return nil
	}
}

// launchBoundAt is the bound a launch attempt reserves under: the startup
// bound with the budget the operator recorded since in place of its own. A
// launch reads it in the transaction that gates it, where it has one, so the
// reservation and the gate see one budget, and a recorded change applies from
// the next admission pass with no restart.
func launchBoundAt(ctx context.Context, tx *store.ReadTx, startup ward.LaunchBound) (ward.LaunchBound, error) {
	budgetMiB, recorded, err := tx.MemoryBudget(ctx)
	if err != nil {
		return ward.LaunchBound{}, err
	}
	if recorded {
		startup.BudgetMiB = budgetMiB
	}
	return startup, nil
}

// currentLaunchBound reads launchBoundAt in a transaction of its own, for a
// path that must know whether a budget applies before it writes anything.
func currentLaunchBound(ctx context.Context, st *store.Store, startup ward.LaunchBound) (ward.LaunchBound, error) {
	var bound ward.LaunchBound
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		bound, err = launchBoundAt(ctx, tx, startup)
		return err
	})
	return bound, err
}

// executionMemory is the reservation an execution admission of run makes,
// read in the transaction that gates it: the writer size the run's policy
// resolves, which is the size the stage driver will declare. It is nil with
// no budget, and for an admission that names no identity, which takes no pool
// slot and starts no ward writer (store.RequireExecutionMemory). Whether it
// fits is the gate's to decide, so a replayed admission that already holds
// its reservation passes under a budget lowered below it.
func (e *Engine) executionMemory(
	ctx context.Context, tx *store.ReadTx, run domain.Run, admission domain.ExecutionAdmission,
) (*store.MemoryReservation, error) {
	if admission.AuthIdentityID == nil {
		return nil, nil
	}
	bound, err := launchBoundAt(ctx, tx, e.launchBound)
	if err != nil || !bound.Budgeted() {
		return nil, err
	}
	policy, err := runResolvedPolicy(ctx, tx, run)
	if err != nil {
		return nil, fmt.Errorf("writer size policy: %w", err)
	}
	sizes, err := ward.ResolveLaunchSizes(policy.Keys, ward.LaunchBound{})
	if err != nil {
		return nil, err
	}
	reservation, _, err := bound.LaunchReservation(
		admission.InvocationID, run.ID, domain.ReservationExecution, sizes.Writer)
	if err != nil {
		return nil, err
	}
	return &store.MemoryReservation{
		BudgetMiB: bound.BudgetMiB, Reservation: reservation, At: time.Now().UTC(),
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
			"Raise the budget with freesided set-memory-budget or lower the policy's size, or stop the task.",
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
// the budget refuses for good. The launch stays pending, so a raised budget
// or a later start whose runtime fits it continues the task; the notice
// converges on the open one across passes and offers stop. A launch that
// waited for memory before the budget was lowered below it no longer waits:
// its machine_capacity hold is cleared, and it leaves the memory order so it
// holds back no launch behind it. Both are read first because the refusal
// repeats every pass and a write would move the revision each time. The
// cleared hold no longer paces the next one in pace, so a wait that returns
// inside the refresh interval is recorded (#727).
func noticeLaunchMemoryRefusal(
	ctx context.Context, st *store.Store, attention attentionService, pace *observationPace,
	runID domain.RunID, invocationID domain.InvocationID, projectID domain.ProjectID, refusal error,
) error {
	var held, waiting bool
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		hold, present, err := tx.GetRunHold(ctx, runID)
		if err != nil {
			return err
		}
		held = present && hold.Reason == domain.HoldMachineCapacity
		waiting, err = tx.LaunchWaiting(ctx, invocationID)
		return err
	}); err != nil {
		return err
	}
	if held || waiting {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.LeaveLaunchWait(ctx, invocationID); err != nil {
				return err
			}
			return tx.ClearRunHoldCause(ctx, runID, domain.HoldMachineCapacity)
		}); err != nil {
			return err
		}
		pace.forget("hold:" + string(runID))
	}
	return recordProductionQuarantine(
		ctx, st, attention, launchMemoryRefusedPrefix, runID, projectID, launchMemoryRefusalReason(refusal))
}

// recordLaunchOrder keeps a launch's place in the memory order in step with
// why it was refused. A launch refused for memory joins the order, or
// refreshes its wait so the gate keeps counting it (store.LaunchWaitRefresh);
// one refused for anything else, such as a pool slot taken since it joined,
// leaves it, so it holds back no launch on another pool. An update drain
// changes nothing: it holds every launch alike, so a waiter keeps its place
// for the lift (issue #3104). A refused launch is retried every pass and
// every write advances the revision, so the order is written only when it
// changes or the
// wait is due a refresh. Between a publication-lane launch's refusals, the
// pass running keeps its wait alive (launchWaitKeeper), so a wait's refused_at
// stays its latest real refusal, which this throttles by. With no budget
// nothing joins, so nothing is written.
// A launch that joins fits the budget, so a notice that a smaller budget
// refused it for good no longer describes its run and is retired.
func recordLaunchOrder(
	ctx context.Context, st *store.Store, attention attentionService,
	runID domain.RunID, invocationID domain.InvocationID, refusal error, now time.Time,
) error {
	if refusal == nil || errors.Is(refusal, store.ErrUpdateDrainInForce) {
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
	if join && !waiting {
		return releaseProductionQuarantine(ctx, st, attention, launchMemoryRefusedPrefix, runID)
	}
	return nil
}

// launchWaitKeeper keeps the production publication lane's memory waits alive
// while one of its passes runs (store.KeepLaunchWaits). The lane is serial, so
// it refuses a launch it waits on again only when a pass reaches that launch's
// task; one long verification of another task would otherwise outlast the
// wait's lease and let a newer launch take memory ahead of it (#2953). A
// daemon that is gone runs no pass, so its waits still lapse.
type launchWaitKeeper struct {
	// mu holds for a whole pass, so lastStart is the start of the pass
	// before this one even when two callers reconcile the lane at once.
	mu        sync.Mutex
	lastStart time.Time
}

// keepDuring runs pass and, until it returns, writes a keepalive about every
// store.LaunchWaitRefresh for the waits refused since the previous pass's
// start minus the lease, or this pass's on the first pass after a start. Each
// tick reads the waits afresh, so a wait first refused earlier in this pass
// is kept from the next tick on. A failed keepalive is logged, not returned:
// it costs a wait at most its place in the order, and the pass itself
// surfaces a broken store. The ticker goroutine is joined before return.
func (k *launchWaitKeeper) keepDuring(
	ctx context.Context, st *store.Store, now func() time.Time, logger *slog.Logger, pass func(),
) {
	k.mu.Lock()
	defer k.mu.Unlock()
	start := now()
	since := k.lastStart
	if since.IsZero() {
		since = start
	}
	k.lastStart = start

	keepCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(store.LaunchWaitRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-keepCtx.Done():
				return
			case <-ticker.C:
			}
			err := st.WriteInternal(keepCtx, func(tx *store.InternalTx) error {
				return tx.KeepLaunchWaits(keepCtx, since, now())
			})
			if err != nil && keepCtx.Err() == nil {
				logger.WarnContext(keepCtx, "launch wait keepalive failed", "error", err)
			}
		}
	}()
	defer func() {
		stop()
		<-done
	}()
	pass()
}
