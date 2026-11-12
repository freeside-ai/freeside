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
INSERT INTO launch_waits (invocation_id, run_id, task_created_at, waiting_since, refused_at)
VALUES (?1, ?2, ?3, ?4, ?4)
ON CONFLICT (invocation_id) DO UPDATE SET refused_at = excluded.refused_at`
	deleteLaunchWaitSQL = `DELETE FROM launch_waits WHERE invocation_id = ?`
	launchWaitSQL       = `SELECT refused_at FROM launch_waits WHERE invocation_id = ?`
	listLaunchWaitsSQL  = `SELECT invocation_id, run_id, task_created_at, refused_at, kept_at FROM launch_waits`
	keepLaunchWaitSQL   = `UPDATE launch_waits SET kept_at = ? WHERE invocation_id = ?`
)

// A launch still pursued is refused, and so refreshes its wait, each time its
// lane retries it, at most once every LaunchWaitRefresh. The production
// publication lane is serial, so while one long pass works on another task it
// retries none of its waits; the engine keeps alive the waits refused recently
// enough that the lane may still pursue them, about every LaunchWaitRefresh,
// until the pass returns (KeepLaunchWaits). The gate drops a wait whose latest refusal
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
// It refuses with a *domain.MachineCapacityError when an older task's launch
// waits for memory ahead of this one, or when the memory other launches hold
// plus this reservation exceeds the budget; nothing is written then, and the
// caller records the wait with RecordLaunchWait. A reservation larger than the
// whole budget is a *domain.LaunchNeverFitsError, never a wait. Otherwise it
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
		ahead, err := tx.launchesAhead(ctx, reservation, holders, at)
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
	if _, err := tx.tx.ExecContext(ctx, putLaunchReservationSQL,
		reservation.InvocationID, reservation.RunID, reservation.Kind,
		reservation.MemoryMiB, reservation.OverheadMiB, formatTime(at)); err != nil {
		return fmt.Errorf("reserve launch %q memory: %w", reservation.InvocationID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, deleteLaunchWaitSQL, reservation.InvocationID); err != nil {
		return fmt.Errorf("end launch %q wait: %w", reservation.InvocationID, err)
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
	err := tx.tx.QueryRowContext(ctx, getLaunchReservationSQL, id).
		Scan(&r.RunID, &r.Kind, &r.MemoryMiB, &r.OverheadMiB)
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
// passes the gate again and starts under it. A verification reservation holds
// until its explicit release.
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
		case domain.ReservationVerification:
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

func (tx *ReadTx) queryLaunchReservations(ctx context.Context, query string) ([]domain.LaunchReservation, error) {
	open, err := queryAll(ctx, tx.tx, func(row scanner) (domain.LaunchReservation, error) {
		var r domain.LaunchReservation
		if err := row.Scan(&r.InvocationID, &r.RunID, &r.Kind, &r.MemoryMiB, &r.OverheadMiB); err != nil {
			return domain.LaunchReservation{}, err
		}
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

// launchWait is one row of the memory order.
type launchWait struct {
	invocation    domain.InvocationID
	run           domain.RunID
	taskCreatedAt time.Time
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

// before orders waits by task submission; the invocation id breaks a tie so
// every gate reads the same order.
func (w launchWait) before(other launchWait) bool {
	if c := w.taskCreatedAt.Compare(other.taskCreatedAt); c != 0 {
		return c < 0
	}
	return w.invocation < other.invocation
}

// launchesAhead counts the waits for memory that come before this launch in
// task submission order. A wait whose launch now holds memory, whose refusal
// and keepalive have both lapsed (launchWaitLease), or whose run can no longer
// launch is no longer in the order; it is deleted here, so a launch nothing
// pursues cannot block everyone behind it.
func (tx *WriteTx) launchesAhead(
	ctx context.Context, reservation domain.LaunchReservation, holders []domain.InvocationID, at time.Time,
) (int, error) {
	createdAt, err := tx.runTaskCreatedAt(ctx, reservation.RunID)
	if err != nil {
		return 0, err
	}
	self := launchWait{invocation: reservation.InvocationID, run: reservation.RunID, taskCreatedAt: createdAt}
	waits, err := tx.listLaunchWaits(ctx)
	if err != nil {
		return 0, err
	}
	ahead := 0
	for _, wait := range waits {
		if wait.invocation == reservation.InvocationID {
			continue
		}
		live := !slices.Contains(holders, wait.invocation) && at.Sub(wait.refreshedAt()) < launchWaitLease
		if live {
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

func (tx *ReadTx) listLaunchWaits(ctx context.Context) ([]launchWait, error) {
	waits, err := queryAll(ctx, tx.tx, func(row scanner) (launchWait, error) {
		var (
			wait                 launchWait
			createdAt, refusedAt string
			keptAt               sql.NullString
			err                  error
		)
		if err := row.Scan(&wait.invocation, &wait.run, &createdAt, &refusedAt, &keptAt); err != nil {
			return launchWait{}, err
		}
		if wait.taskCreatedAt, err = parseTime(createdAt); err != nil {
			return launchWait{}, fmt.Errorf("launch wait %q task_created_at: %w", wait.invocation, err)
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

// RecordLaunchWait places a launch the memory gate refused at at in the order,
// or refreshes its wait. It runs in its own transaction after the refusal, so
// a launch with no row yet waits one more pass at most. Recording again keeps
// the row's place and moves only its refusal; the order is by task
// submission, so leaving and rejoining loses no place.
func (tx *WriteTx) RecordLaunchWait(
	ctx context.Context, invocation domain.InvocationID, runID domain.RunID, at time.Time,
) error {
	createdAt, err := tx.runTaskCreatedAt(ctx, runID)
	if err != nil {
		return fmt.Errorf("record launch %q wait: %w", invocation, err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordLaunchWaitSQL,
		invocation, runID, formatTime(createdAt), formatTime(at)); err != nil {
		return fmt.Errorf("record launch %q wait: %w", invocation, err)
	}
	return nil
}

// KeepLaunchWaits keeps alive, at at, the waits a running production
// publication pass may still be pursuing: those last refused at or after since
// minus launchWaitLease, where since is the start of the previous pass, or of
// this one when it is the first. The set does not tell lanes apart, so it also
// holds dispatch-loop waits. A wait the lane pursues is refused again in every
// pass that reaches its task, so it stays in that set; one nothing pursues
// drops out after two passes at most and then lapses. A wait already lapsed at
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
		if wait.refusedAt.Before(pursuedSince) || at.Sub(wait.refreshedAt()) >= launchWaitLease {
			continue
		}
		if _, err := tx.tx.ExecContext(ctx, keepLaunchWaitSQL, formatTime(at), wait.invocation); err != nil {
			return fmt.Errorf("keep launch %q wait: %w", wait.invocation, err)
		}
	}
	return nil
}

// LaunchWaiting reports whether a launch is in the memory order.
func (tx *ReadTx) LaunchWaiting(ctx context.Context, invocation domain.InvocationID) (bool, error) {
	_, waiting, err := tx.LaunchWaitRefusedAt(ctx, invocation)
	return waiting, err
}

// LaunchWaitRefusedAt reports whether a launch is in the memory order and the
// latest refusal its wait records. A keepalive (KeepLaunchWaits) is not a
// refusal and does not move it.
func (tx *ReadTx) LaunchWaitRefusedAt(
	ctx context.Context, invocation domain.InvocationID,
) (time.Time, bool, error) {
	var refusedAt string
	err := tx.tx.QueryRowContext(ctx, launchWaitSQL, invocation).Scan(&refusedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("launch %q wait: %w", invocation, err)
	}
	at, err := parseTime(refusedAt)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("launch %q wait refused_at: %w", invocation, err)
	}
	return at, true, nil
}

// LeaveLaunchWait removes a launch from the memory order because something
// other than memory refused it, such as a pool slot taken since it joined.
// It rejoins at its task's place once only memory refuses it again.
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
