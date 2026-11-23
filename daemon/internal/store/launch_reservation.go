package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	listOpenLaunchReservationsSQL = `
SELECT invocation_id, run_id, kind, memory_mib, overhead_mib
FROM launch_reservations WHERE released_at IS NULL`
	getLaunchReservationSQL = `
SELECT run_id, kind, memory_mib, overhead_mib FROM launch_reservations WHERE invocation_id = ?`
	// Re-reserving an invocation reopens its row: a dispatch that went back to
	// pending, or a verification job run again, holds memory again, under the
	// overhead of the runtime it launches on now.
	putLaunchReservationSQL = `
INSERT INTO launch_reservations (invocation_id, run_id, kind, memory_mib, overhead_mib, reserved_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (invocation_id) DO UPDATE SET
    overhead_mib = excluded.overhead_mib,
    reserved_at = CASE WHEN released_at IS NULL THEN reserved_at ELSE excluded.reserved_at END,
    released_at = NULL`
	releaseLaunchReservationSQL = `
UPDATE launch_reservations SET released_at = ? WHERE invocation_id = ? AND released_at IS NULL`
	releaseTaskVerificationReservationsSQL = `
UPDATE launch_reservations SET released_at = ?1
WHERE kind = 'verification' AND released_at IS NULL
  AND run_id IN (SELECT id FROM runs WHERE task_id = ?2)`
	recordLaunchWaitSQL = `
INSERT INTO launch_waits (invocation_id, run_id, task_created_at, waiting_since, refused_at, resource)
VALUES (?1, ?2, ?3, ?4, ?4, ?5)
ON CONFLICT (invocation_id) DO UPDATE SET refused_at = excluded.refused_at, resource = excluded.resource`
	deleteLaunchWaitSQL = `DELETE FROM launch_waits WHERE invocation_id = ?`
	launchWaitSQL       = `SELECT refused_at, resource FROM launch_waits WHERE invocation_id = ?`
	listLaunchWaitsSQL  = `
SELECT invocation_id, run_id, task_created_at, waiting_since, refused_at, kept_at, resource FROM launch_waits`
	keepLaunchWaitSQL = `UPDATE launch_waits SET kept_at = ? WHERE invocation_id = ?`
	// A daemon wait names no run and no task; it keeps its first waiting_since
	// across refreshes, as a run's wait keeps its place.
	recordDaemonLaunchWaitSQL = `
INSERT INTO launch_waits (invocation_id, run_id, task_created_at, waiting_since, refused_at, resource)
VALUES (?1, NULL, NULL, ?2, ?2, 'memory')
ON CONFLICT (invocation_id) DO UPDATE SET refused_at = excluded.refused_at
WHERE launch_waits.run_id IS NULL`
	daemonLaunchWaitSinceSQL  = `SELECT waiting_since FROM launch_waits WHERE invocation_id = ? AND run_id IS NULL`
	clearDaemonLaunchWaitsSQL = `DELETE FROM launch_waits WHERE run_id IS NULL`
)

// A launch still pursued is refused, and so refreshes its wait, each time its
// lane retries it, at most once every LaunchWaitRefresh. The production
// publication lane runs a run's tasks one after another, so while one long
// task works it retries none of its run's other waits; the engine keeps alive
// the waits refused recently enough that the lane may still pursue them,
// about every LaunchWaitRefresh, until no task of the lane runs
// (KeepLaunchWaits). The gate drops a wait whose latest refusal
// and latest keepalive are both older than launchWaitLease, because nothing
// pursues that launch any more: its review failed for good, its task was
// blocked, its shadow arm was abandoned, or its daemon is gone and writes
// neither. A lapsed wait costs only its place in the order, never the budget.
const (
	LaunchWaitRefresh = time.Minute
	launchWaitLease   = 5 * LaunchWaitRefresh
)

// RequireMachineCapacity is the memory half of a launch's scheduling gate
// (plan §5.4). It runs in the transaction that writes the launch's admission,
// after the usage-pool gate, so a launch waiting on its pool slot never joins
// the memory order. budgetMiB is the instance budget, and the caller computed
// the reservation from the launch's declared size and the runtime's overhead.
//
// It refuses with a *domain.MachineCapacityError when a wait for memory comes
// ahead of this one, or when the memory other launches hold plus this
// reservation exceeds the budget; nothing is written then, and the caller
// records the wait with RecordLaunchWait, or RecordDaemonLaunchWait for the
// daemon's own work. A reservation larger than the whole budget is a
// *domain.LaunchNeverFitsError, never a wait.
//
// The order has two tiers. The daemon's own work (a kind that belongs to no
// run, such as a proof pass or an image build) waits ahead of every task's
// launch, and such waits are granted among themselves in the order they first
// asked. Task launches follow, in their tasks' submission order. A task launch
// never passes a waiting daemon reservation, even when it would fit: the
// daemon's own work has no task to order it by, and it is often what a
// waiting launch needs, such as the proof that admits it or the image it runs
// in, so a launch ahead of it could wait for it for ever. A daemon
// reservation does pass a waiting task launch. The rule holds whatever order
// a launch path checks its other gates in. Otherwise it
// writes the reservation and ends the launch's wait. A launch that already
// holds its reservation, such as an admitted execution replayed after a crash,
// passes without a new decision, under the overhead it reserved even when the
// runtime's has changed since, and under a budget lowered below it since: its
// VM may already run at that size, and a budget limits only new reservations.
func (tx *WriteTx) RequireMachineCapacity(
	ctx context.Context, budgetMiB int, reservation domain.LaunchReservation, at time.Time,
) error {
	if err := reservation.Validate(); err != nil {
		return fmt.Errorf("reserve launch memory: %w", err)
	}
	if budgetMiB < 1 {
		return fmt.Errorf("reserve launch %q memory: budget %d MiB: %w",
			reservation.InvocationID, budgetMiB, domain.ErrNonPositive)
	}
	reserved, holders, err := tx.reservedMemory(ctx, at)
	if err != nil {
		return fmt.Errorf("reserve launch %q memory: %w", reservation.InvocationID, err)
	}
	stored, err := tx.requireReservationUnchanged(ctx, reservation)
	if err != nil {
		return err
	}
	held := slices.Contains(holders, reservation.InvocationID)
	if held {
		// Holding implies an open stored row, already counted in reserved.
		reservation = stored
	}
	if !held {
		if reservation.TotalMiB() > budgetMiB {
			return &domain.LaunchNeverFitsError{
				SizeMiB: reservation.MemoryMiB, OverheadMiB: reservation.OverheadMiB, BudgetMiB: budgetMiB,
			}
		}
		self, err := tx.selfWait(ctx, reservation, at)
		if err != nil {
			return fmt.Errorf("reserve launch %q memory: %w", reservation.InvocationID, err)
		}
		ahead, err := tx.launchesAhead(ctx, LaunchWaitMemory, self, holders, at)
		if err != nil {
			return fmt.Errorf("reserve launch %q memory: %w", reservation.InvocationID, err)
		}
		if ahead > 0 || reserved+reservation.TotalMiB() > budgetMiB {
			return &domain.MachineCapacityError{Hold: domain.MachineCapacityHold{
				ReservationMiB: reservation.TotalMiB(), ReservedMiB: reserved,
				BudgetMiB: budgetMiB, Ahead: ahead,
			}}
		}
	}
	if err := tx.putLaunchReservation(ctx, reservation, at); err != nil {
		return err
	}
	if _, err := tx.tx.ExecContext(ctx, deleteLaunchWaitSQL, reservation.InvocationID); err != nil {
		return fmt.Errorf("end launch %q wait: %w", reservation.InvocationID, err)
	}
	return nil
}

// putLaunchReservation writes the gate's granted reservation, storing a
// daemon reservation's empty run as NULL.
func (tx *WriteTx) putLaunchReservation(ctx context.Context, r domain.LaunchReservation, at time.Time) error {
	if _, err := tx.tx.ExecContext(ctx, putLaunchReservationSQL,
		r.InvocationID, nullableRun(r.RunID), r.Kind, r.MemoryMiB, r.OverheadMiB, formatTime(at)); err != nil {
		return fmt.Errorf("reserve launch %q memory: %w", r.InvocationID, err)
	}
	return nil
}

// MemoryReservation is a launch's memory reservation and the budget it is
// gated against (RequireMachineCapacity).
type MemoryReservation struct {
	BudgetMiB   int
	Reservation domain.LaunchReservation
	At          time.Time
}

// RequireExecutionMemory applies the memory gate to an execution admission
// that takes a pool slot; provider-free clean verification carries no
// identity, starts no ward writer, and reserves nothing, as for the pool. A
// replayed admission whose dispatch reservation stands already holds its
// memory and passes. A nil memory reserves nothing.
func (tx *WriteTx) RequireExecutionMemory(
	ctx context.Context, admission domain.ExecutionAdmission, memory *MemoryReservation,
) error {
	if memory == nil || admission.AuthIdentityID == nil {
		return nil
	}
	if memory.Reservation.InvocationID != admission.InvocationID || memory.Reservation.RunID != admission.RunID ||
		memory.Reservation.Kind != domain.ReservationExecution {
		return fmt.Errorf("reserve launch %q memory for admission %q of run %q: %w",
			memory.Reservation.InvocationID, admission.InvocationID, admission.RunID, domain.ErrParentKeyMismatch)
	}
	return tx.RequireMachineCapacity(ctx, memory.BudgetMiB, memory.Reservation, memory.At)
}

// requireReservationUnchanged refuses to re-reserve an invocation under a
// different run, kind, or declared size: one launch has one declared size, and
// a changed one means the caller disagrees with the record. The overhead is
// the runtime's, not the launch's, so it may differ. It returns the stored
// row, or the zero value when there is none.
func (tx *WriteTx) requireReservationUnchanged(
	ctx context.Context, reservation domain.LaunchReservation,
) (domain.LaunchReservation, error) {
	stored, found, err := tx.getLaunchReservation(ctx, reservation.InvocationID)
	if err != nil || !found {
		return domain.LaunchReservation{}, err
	}
	asked := reservation
	asked.OverheadMiB = stored.OverheadMiB
	if stored != asked {
		return domain.LaunchReservation{}, fmt.Errorf("reserve launch %q memory: stored %+v, asked %+v: %w",
			reservation.InvocationID, stored, reservation, ErrImmutableConflict)
	}
	return stored, nil
}

func (tx *ReadTx) getLaunchReservation(
	ctx context.Context, id domain.InvocationID,
) (domain.LaunchReservation, bool, error) {
	r := domain.LaunchReservation{InvocationID: id}
	var run sql.NullString
	err := tx.tx.QueryRowContext(ctx, getLaunchReservationSQL, id).
		Scan(&run, &r.Kind, &r.MemoryMiB, &r.OverheadMiB)
	r.RunID = domain.RunID(run.String)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.LaunchReservation{}, false, nil
	}
	if err != nil {
		return domain.LaunchReservation{}, false, fmt.Errorf("get launch reservation %q: %w", id, err)
	}
	if err := r.Validate(); err != nil {
		return domain.LaunchReservation{}, false, fmt.Errorf("get launch reservation %q: %w", id, err)
	}
	return r, true, nil
}

// reservedMemory sums the reservations that still hold memory and returns the
// invocations behind them. An execution or review reservation holds exactly
// while its admission holds its usage-pool slot, because both end at the same
// proof that every container is absent. A row whose launch ended for good is
// marked released so later gates skip it; an execution whose dispatch went
// back to pending holds nothing now but keeps its row, because its replay
// passes the gate again and starts under it. A verification reservation, and
// one for the daemon's own work, holds until its explicit release.
func (tx *WriteTx) reservedMemory(ctx context.Context, at time.Time) (int, []domain.InvocationID, error) {
	open, err := tx.openLaunchReservations(ctx)
	if err != nil {
		return 0, nil, err
	}
	executions, err := tx.executionSlotsHeld(ctx, "")
	if err != nil {
		return 0, nil, err
	}
	journals, err := queryIDs[domain.InvocationID](ctx, &tx.ReadTx, listOpenHandoffHoldersSQL)
	if err != nil {
		return 0, nil, fmt.Errorf("list open handoffs: %w", err)
	}
	total := 0
	var holders []domain.InvocationID
	for _, r := range open {
		held, ended := true, false
		switch r.Kind {
		case domain.ReservationExecution:
			held = slices.Contains(executions, r.InvocationID)
			if !held {
				terminal, err := tx.executionTerminal(ctx, r.InvocationID)
				if err != nil {
					return 0, nil, err
				}
				ended = terminal && !slices.Contains(journals, r.InvocationID)
			}
		case domain.ReservationReview:
			held, err = tx.reviewSlotHeld(ctx, r.InvocationID)
			if err != nil {
				return 0, nil, err
			}
			ended = !held
		case domain.ReservationVerification, domain.ReservationImageBuild, domain.ReservationImageRegistry,
			domain.ReservationProofPass, domain.ReservationProbe:
		}
		if ended {
			if _, err := tx.tx.ExecContext(ctx, releaseLaunchReservationSQL, formatTime(at), r.InvocationID); err != nil {
				return 0, nil, fmt.Errorf("release ended launch %q: %w", r.InvocationID, err)
			}
		}
		if !held {
			continue
		}
		total += r.TotalMiB()
		holders = append(holders, r.InvocationID)
	}
	return total, holders, nil
}

func (tx *ReadTx) openLaunchReservations(ctx context.Context) ([]domain.LaunchReservation, error) {
	return tx.queryLaunchReservations(ctx, listOpenLaunchReservationsSQL)
}

// ListOpenVerificationReservations returns every verification reservation
// not yet released, in invocation order. The engine's end-of-task proof and
// the operator release read it to find a reservation whose task has ended.
func (tx *ReadTx) ListOpenVerificationReservations(ctx context.Context) ([]domain.LaunchReservation, error) {
	return tx.queryLaunchReservations(ctx,
		listOpenLaunchReservationsSQL+` AND kind = 'verification' ORDER BY invocation_id`)
}

// ListOpenDaemonReservations returns every reservation for the daemon's own
// work not yet released, in invocation order. A start reads it to release
// what an earlier process left open, once that work's containers are proved
// gone, before any site reserves again.
func (tx *ReadTx) ListOpenDaemonReservations(ctx context.Context) ([]domain.LaunchReservation, error) {
	return tx.queryLaunchReservations(ctx,
		listOpenLaunchReservationsSQL+` AND run_id IS NULL ORDER BY invocation_id`)
}

func (tx *ReadTx) queryLaunchReservations(ctx context.Context, query string) ([]domain.LaunchReservation, error) {
	open, err := queryAll(ctx, tx.tx, func(row scanner) (domain.LaunchReservation, error) {
		var (
			r   domain.LaunchReservation
			run sql.NullString
		)
		if err := row.Scan(&r.InvocationID, &run, &r.Kind, &r.MemoryMiB, &r.OverheadMiB); err != nil {
			return domain.LaunchReservation{}, err
		}
		r.RunID = domain.RunID(run.String)
		// A row that does not validate fails the gate closed rather than
		// counting a size no launch declared.
		if err := r.Validate(); err != nil {
			return domain.LaunchReservation{}, err
		}
		return r, nil
	}, query)
	if err != nil {
		return nil, fmt.Errorf("list launch reservations: %w", err)
	}
	return open, nil
}

// LaunchWaitResource names the order a launch waits in (#3471): memory
// under the instance's budget, or a place under the cordoned launch cap.
// Each gate counts only its own resource's waits, so a cap waiter never
// holds back a warded launch and a memory waiter never holds back a
// cordoned one. A wait for the daemon's own work is always a memory wait.
type LaunchWaitResource string

const (
	LaunchWaitMemory         LaunchWaitResource = "memory"
	LaunchWaitCordonedLaunch LaunchWaitResource = "cordoned_launch"
)

// ErrInvalidLaunchWaitResource refuses a wait resource outside
// AllLaunchWaitResources, on write and on decode.
var ErrInvalidLaunchWaitResource = errors.New("invalid launch wait resource")

// AllLaunchWaitResources is the single registration point for wait
// resources.
var AllLaunchWaitResources = []LaunchWaitResource{LaunchWaitMemory, LaunchWaitCordonedLaunch}

func (r LaunchWaitResource) valid() bool {
	switch r {
	case LaunchWaitMemory, LaunchWaitCordonedLaunch:
		return true
	default:
		return false
	}
}

// launchWait is one row of a launch order. A wait for the daemon's own work
// names no run, and its taskCreatedAt is zero.
type launchWait struct {
	invocation    domain.InvocationID
	resource      LaunchWaitResource
	run           domain.RunID
	taskCreatedAt time.Time
	waitingSince  time.Time
	refusedAt     time.Time
	// keptAt is the latest keepalive a publication pass wrote, zero when none.
	keptAt time.Time
}

// refreshedAt is the latest refusal or keepalive, which the lease runs from.
func (w launchWait) refreshedAt() time.Time {
	if w.keptAt.After(w.refusedAt) {
		return w.keptAt
	}
	return w.refusedAt
}

// daemon reports whether the wait is for the daemon's own work.
func (w launchWait) daemon() bool { return w.run == "" }

// before is the memory order (RequireMachineCapacity): the daemon's own waits
// ahead of every task launch, by when they first asked, then task launches by
// task submission. The invocation id breaks a tie so every gate reads the
// same order.
func (w launchWait) before(other launchWait) bool {
	if w.daemon() != other.daemon() {
		return w.daemon()
	}
	first, second := w.taskCreatedAt, other.taskCreatedAt
	if w.daemon() {
		first, second = w.waitingSince, other.waitingSince
	}
	if c := first.Compare(second); c != 0 {
		return c < 0
	}
	return w.invocation < other.invocation
}

// launchesAhead counts the waits for resource that come before self in that
// resource's order (launchWait.before). A wait whose launch is now a holder,
// whose refusal and keepalive have both lapsed (launchWaitLease), or whose run
// can no longer launch is no longer in the order; it is deleted here, so a
// launch nothing pursues cannot block everyone behind it. A daemon wait has no
// run to ask, so only its lease ends it. Another resource's waits are neither
// counted nor ended here; their own gate ends them.
func (tx *WriteTx) launchesAhead(
	ctx context.Context, resource LaunchWaitResource, self launchWait, holders []domain.InvocationID, at time.Time,
) (int, error) {
	waits, err := tx.listLaunchWaits(ctx)
	if err != nil {
		return 0, err
	}
	ahead := 0
	for _, wait := range waits {
		if wait.resource != resource || wait.invocation == self.invocation {
			continue
		}
		live := !slices.Contains(holders, wait.invocation) && at.Sub(wait.refreshedAt()) < launchWaitLease
		if live && !wait.daemon() {
			if live, err = tx.reviewRunCanLaunch(ctx, wait.run); err != nil {
				return 0, err
			}
		}
		if !live {
			if _, err := tx.tx.ExecContext(ctx, deleteLaunchWaitSQL, wait.invocation); err != nil {
				return 0, fmt.Errorf("drop ended launch wait %q: %w", wait.invocation, err)
			}
			continue
		}
		if wait.before(self) {
			ahead++
		}
	}
	return ahead, nil
}

// selfWait is the place this reservation asks from in the memory order. A
// launch asks from its task's submission. The daemon's own work asks from when it first waited, or
// from at when it has not waited yet, so a daemon reservation asking for the
// first time comes after every daemon wait already recorded.
func (tx *WriteTx) selfWait(
	ctx context.Context, reservation domain.LaunchReservation, at time.Time,
) (launchWait, error) {
	self := launchWait{invocation: reservation.InvocationID, resource: LaunchWaitMemory, run: reservation.RunID}
	if reservation.Kind.ForRun() {
		createdAt, err := tx.runTaskCreatedAt(ctx, reservation.RunID)
		self.taskCreatedAt = createdAt
		return self, err
	}
	var since string
	err := tx.tx.QueryRowContext(ctx, daemonLaunchWaitSinceSQL, reservation.InvocationID).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) {
		self.waitingSince = at
		return self, nil
	}
	if err != nil {
		return launchWait{}, fmt.Errorf("launch %q wait: %w", reservation.InvocationID, err)
	}
	if self.waitingSince, err = parseTime(since); err != nil {
		return launchWait{}, fmt.Errorf("launch wait %q waiting_since: %w", reservation.InvocationID, err)
	}
	return self, nil
}

func (tx *ReadTx) listLaunchWaits(ctx context.Context) ([]launchWait, error) {
	waits, err := queryAll(ctx, tx.tx, func(row scanner) (launchWait, error) {
		var (
			wait                    launchWait
			run, createdAt, keptAt  sql.NullString
			waitingSince, refusedAt string
			err                     error
		)
		if err := row.Scan(
			&wait.invocation, &run, &createdAt, &waitingSince, &refusedAt, &keptAt, &wait.resource,
		); err != nil {
			return launchWait{}, err
		}
		if !wait.resource.valid() {
			return launchWait{}, fmt.Errorf("launch wait %q resource %q: %w",
				wait.invocation, wait.resource, ErrInvalidLaunchWaitResource)
		}
		wait.run = domain.RunID(run.String)
		// The table ties the two columns: both name a run's wait, or neither.
		if createdAt.Valid {
			if wait.taskCreatedAt, err = parseTime(createdAt.String); err != nil {
				return launchWait{}, fmt.Errorf("launch wait %q task_created_at: %w", wait.invocation, err)
			}
		}
		if wait.waitingSince, err = parseTime(waitingSince); err != nil {
			return launchWait{}, fmt.Errorf("launch wait %q waiting_since: %w", wait.invocation, err)
		}
		if wait.refusedAt, err = parseTime(refusedAt); err != nil {
			return launchWait{}, fmt.Errorf("launch wait %q refused_at: %w", wait.invocation, err)
		}
		if keptAt.Valid {
			if wait.keptAt, err = parseTime(keptAt.String); err != nil {
				return launchWait{}, fmt.Errorf("launch wait %q kept_at: %w", wait.invocation, err)
			}
		}
		return wait, nil
	}, listLaunchWaitsSQL)
	if err != nil {
		return nil, fmt.Errorf("list launch waits: %w", err)
	}
	return waits, nil
}

// runTaskCreatedAt is the submission instant that places a run's launches in
// the memory order. A run without a task never launches.
func (tx *ReadTx) runTaskCreatedAt(ctx context.Context, runID domain.RunID) (time.Time, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return time.Time{}, err
	}
	if run.TaskID == "" {
		return time.Time{}, fmt.Errorf("run %q has no task to order its launch by: %w", runID, ErrNotFound)
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return time.Time{}, err
	}
	return task.CreatedAt, nil
}

// RecordLaunchWait places a launch the resource's gate refused at at in that
// resource's order, or refreshes its wait. It runs in its own transaction
// after the refusal, so a launch with no row yet waits one more pass at
// most. Recording again keeps the row's place and moves only its refusal
// and resource; the order is by task submission, so leaving and rejoining
// loses no place.
func (tx *WriteTx) RecordLaunchWait(
	ctx context.Context, invocation domain.InvocationID, runID domain.RunID, resource LaunchWaitResource, at time.Time,
) error {
	if !resource.valid() {
		return fmt.Errorf("record launch %q wait on %q: %w", invocation, resource, ErrInvalidLaunchWaitResource)
	}
	createdAt, err := tx.runTaskCreatedAt(ctx, runID)
	if err != nil {
		return fmt.Errorf("record launch %q wait: %w", invocation, err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordLaunchWaitSQL,
		invocation, runID, formatTime(createdAt), formatTime(at), resource); err != nil {
		return fmt.Errorf("record launch %q wait: %w", invocation, err)
	}
	return nil
}

// RecordDaemonLaunchWait places a reservation for the daemon's own work that
// the memory gate refused at at in the order, or refreshes its wait. Its
// place is when it first waited, kept across refreshes, ahead of every task
// launch (RequireMachineCapacity). The waiter refreshes it at least every
// LaunchWaitRefresh while it waits, so a wait whose waiter is gone lapses
// with the lease. An invocation that already waits as a run's launch keeps
// that wait unchanged.
func (tx *WriteTx) RecordDaemonLaunchWait(ctx context.Context, invocation domain.InvocationID, at time.Time) error {
	if invocation == "" {
		return fmt.Errorf("record daemon launch wait: %w", domain.ErrEmptyID)
	}
	if _, err := tx.tx.ExecContext(ctx, recordDaemonLaunchWaitSQL, invocation, formatTime(at)); err != nil {
		return fmt.Errorf("record daemon launch %q wait: %w", invocation, err)
	}
	return nil
}

// ClearDaemonLaunchWaits removes every wait for the daemon's own work. A
// start calls it before any site reserves, because a wait an earlier process
// left has no waiter now and would otherwise hold back every task launch
// until its lease lapsed.
func (tx *WriteTx) ClearDaemonLaunchWaits(ctx context.Context) error {
	if _, err := tx.tx.ExecContext(ctx, clearDaemonLaunchWaitsSQL); err != nil {
		return fmt.Errorf("clear daemon launch waits: %w", err)
	}
	return nil
}

// KeepLaunchWaits keeps alive, at at, the waits a running production
// publication task may still be pursuing: those last refused at or after since
// minus launchWaitLease, where since is the start of the pass before the one
// that started the earliest running task, or of that pass when it is the
// first. The set does not tell lanes apart, so it also
// holds dispatch-loop waits, but never a daemon wait, which its own waiter
// refreshes. A wait the lane pursues is refused again in every pass that
// reaches its task, so it stays in that set; one nothing pursues
// drops out once every running task started at least two passes after its
// last refusal, and then lapses. A wait already lapsed at
// at stays lapsed: a keepalive extends a live wait, never revives one. The
// keepalive moves only kept_at, never refused_at, so it cannot carry itself
// forward from one pass to the next. Waits are not synchronized state, so this writes no
// client revision. Times are compared parsed, not as text, because
// RFC3339Nano trims trailing zeros.
func (tx *InternalTx) KeepLaunchWaits(ctx context.Context, since, at time.Time) error {
	waits, err := tx.listLaunchWaits(ctx)
	if err != nil {
		return fmt.Errorf("keep launch waits: %w", err)
	}
	pursuedSince := since.Add(-launchWaitLease)
	for _, wait := range waits {
		// A daemon wait's own waiter refreshes it; the lane pursues none.
		if wait.daemon() || wait.refusedAt.Before(pursuedSince) || at.Sub(wait.refreshedAt()) >= launchWaitLease {
			continue
		}
		if _, err := tx.tx.ExecContext(ctx, keepLaunchWaitSQL, formatTime(at), wait.invocation); err != nil {
			return fmt.Errorf("keep launch %q wait: %w", wait.invocation, err)
		}
	}
	return nil
}

// LaunchWaiting reports whether a launch is in a launch order.
func (tx *ReadTx) LaunchWaiting(ctx context.Context, invocation domain.InvocationID) (bool, error) {
	_, waiting, err := tx.LaunchWaitRefusedAt(ctx, invocation)
	return waiting, err
}

// LaunchWaitRefusedAt reports whether a launch is in a launch order and the
// latest refusal its wait records. A keepalive (KeepLaunchWaits) is not a
// refusal and does not move it.
func (tx *ReadTx) LaunchWaitRefusedAt(
	ctx context.Context, invocation domain.InvocationID,
) (time.Time, bool, error) {
	at, _, waiting, err := tx.LaunchWait(ctx, invocation)
	return at, waiting, err
}

// LaunchWait reports whether a launch is in a launch order, the latest
// refusal its wait records, and the resource it waits on.
func (tx *ReadTx) LaunchWait(
	ctx context.Context, invocation domain.InvocationID,
) (time.Time, LaunchWaitResource, bool, error) {
	var (
		refusedAt string
		resource  LaunchWaitResource
	)
	err := tx.tx.QueryRowContext(ctx, launchWaitSQL, invocation).Scan(&refusedAt, &resource)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, "", false, nil
	}
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("launch %q wait: %w", invocation, err)
	}
	if !resource.valid() {
		return time.Time{}, "", false, fmt.Errorf("launch %q wait resource %q: %w",
			invocation, resource, ErrInvalidLaunchWaitResource)
	}
	at, err := parseTime(refusedAt)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("launch %q wait refused_at: %w", invocation, err)
	}
	return at, resource, true, nil
}

// LeaveLaunchWait removes a launch from its launch order because something
// other than its resource refused it, such as a pool slot taken since it
// joined. It rejoins at its task's place once only its resource refuses it
// again.
func (tx *WriteTx) LeaveLaunchWait(ctx context.Context, invocation domain.InvocationID) error {
	if _, err := tx.tx.ExecContext(ctx, deleteLaunchWaitSQL, invocation); err != nil {
		return fmt.Errorf("leave launch %q wait: %w", invocation, err)
	}
	return nil
}

// ReleaseVerificationReservation ends a verification job's reservation once
// every container of its room is proved absent. Releasing one never reserved,
// or already released, does nothing.
func (tx *WriteTx) ReleaseVerificationReservation(
	ctx context.Context, invocation domain.InvocationID, at time.Time,
) error {
	if _, err := tx.tx.ExecContext(ctx,
		releaseLaunchReservationSQL+` AND kind = 'verification'`, formatTime(at), invocation); err != nil {
		return fmt.Errorf("release verification %q reservation: %w", invocation, err)
	}
	return nil
}

// ReleaseDaemonReservation ends a reservation for the daemon's own work once
// every container it started is proved absent. Releasing one never reserved,
// already released, or of a run's kind does nothing.
func (tx *WriteTx) ReleaseDaemonReservation(ctx context.Context, invocation domain.InvocationID, at time.Time) error {
	if _, err := tx.tx.ExecContext(ctx,
		releaseLaunchReservationSQL+` AND run_id IS NULL`, formatTime(at), invocation); err != nil {
		return fmt.Errorf("release daemon reservation %q: %w", invocation, err)
	}
	return nil
}

// nullableRun stores a daemon reservation's empty run as NULL.
func nullableRun(run domain.RunID) any {
	if run == "" {
		return nil
	}
	return string(run)
}
