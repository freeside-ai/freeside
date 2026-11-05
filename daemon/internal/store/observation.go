package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Run observation rows (migration 0024) are the operator-facing projection
// of run progress: written beside the workflow facts, never read by them.
// They are synchronized facts now that the runs list and timeline expose
// them, so every write lives on WriteTx and advances the server revision.
// The read surface re-validates every row and fails closed, so a row the
// current vocabulary cannot express is an error, never a silently weaker
// observation (issues #394 and #657; the reconstruction-gate convention).

const (
	appendRunMilestoneSQL = `INSERT INTO run_milestones
		(run_id, kind, invocation_id, terminal, outcome, reason, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`
	listRunMilestonesSQL = `SELECT run_id, kind, invocation_id, terminal,
			outcome, reason, recorded_at
		FROM run_milestones WHERE run_id = ? ORDER BY id`
	selectRunHoldMergeSQL = `SELECT reason, invocation_id, first_observed_at
		FROM run_hold_observations WHERE run_id = ?`
	upsertInvocationObservationSQL = `INSERT INTO invocation_observations
		(invocation_id, run_id, status, live, observed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (invocation_id) DO UPDATE SET
			run_id = excluded.run_id,
			status = excluded.status,
			live = excluded.live,
			observed_at = excluded.observed_at`
	listInvocationObservationsSQL = `SELECT invocation_id, run_id, status,
			live, observed_at
		FROM invocation_observations WHERE run_id = ? ORDER BY invocation_id`
	selectRunHoldSQL = `SELECT run_id, invocation_id, reason,
			detail_check, detail_agent, detail_launch_stage, detail_launch_digest,
			upkeep_pending, upkeep_refusal_id, upkeep_item_id, upkeep_task_id,
			usage_pool, quota_pool, quota_window, quota_source, quota_invocation_id,
			quota_reading_observed_at, quota_resets_at, quota_set_at, quota_alternate_agents,
			capacity_reservation_mib, capacity_reserved_mib, capacity_budget_mib, capacity_ahead,
			first_observed_at, last_observed_at
		FROM run_hold_observations WHERE run_id = ?`
	replaceRunHoldSQL = `INSERT INTO run_hold_observations
		(run_id, invocation_id, reason,
			detail_check, detail_agent, detail_launch_stage, detail_launch_digest,
			upkeep_pending, upkeep_refusal_id, upkeep_item_id, upkeep_task_id,
			usage_pool, quota_pool, quota_window, quota_source, quota_invocation_id,
			quota_reading_observed_at, quota_resets_at, quota_set_at, quota_alternate_agents,
			capacity_reservation_mib, capacity_reserved_mib, capacity_budget_mib, capacity_ahead,
			first_observed_at, last_observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id) DO UPDATE SET
			invocation_id = excluded.invocation_id,
			reason = excluded.reason,
			detail_check = excluded.detail_check,
			detail_agent = excluded.detail_agent,
			detail_launch_stage = excluded.detail_launch_stage,
			detail_launch_digest = excluded.detail_launch_digest,
			upkeep_pending = excluded.upkeep_pending,
			upkeep_refusal_id = excluded.upkeep_refusal_id,
			upkeep_item_id = excluded.upkeep_item_id,
			upkeep_task_id = excluded.upkeep_task_id,
			usage_pool = excluded.usage_pool,
			quota_pool = excluded.quota_pool,
			quota_window = excluded.quota_window,
			quota_source = excluded.quota_source,
			quota_invocation_id = excluded.quota_invocation_id,
			quota_reading_observed_at = excluded.quota_reading_observed_at,
			quota_resets_at = excluded.quota_resets_at,
			quota_set_at = excluded.quota_set_at,
			quota_alternate_agents = excluded.quota_alternate_agents,
			capacity_reservation_mib = excluded.capacity_reservation_mib,
			capacity_reserved_mib = excluded.capacity_reserved_mib,
			capacity_budget_mib = excluded.capacity_budget_mib,
			capacity_ahead = excluded.capacity_ahead,
			first_observed_at = excluded.first_observed_at,
			last_observed_at = excluded.last_observed_at`
	clearRunHoldSQL      = `DELETE FROM run_hold_observations WHERE run_id = ?`
	clearRunHoldCauseSQL = `DELETE FROM run_hold_observations
		WHERE run_id = ? AND reason = ?`
)

// AppendRunMilestone appends one milestone, first observation wins: a replay
// or crash-retry that re-observes the same (run, kind, invocation) converges
// on the already-recorded instant instead of duplicating or erroring. A
// milestone that actually inserts is forward progress and clears the run's
// current hold; a converged replay clears nothing, because no progress
// happened now and a standing hold must not blink out under replays.
func (tx *WriteTx) AppendRunMilestone(ctx context.Context, m domain.RunMilestone) error {
	if err := m.Validate(); err != nil {
		return fmt.Errorf("append run milestone: %w", err)
	}
	var invocation any
	if m.InvocationID != nil {
		invocation = string(*m.InvocationID)
	}
	var terminal any
	if m.Terminal != nil {
		terminal = string(*m.Terminal)
	}
	var outcome any
	if m.Outcome != nil {
		outcome = string(*m.Outcome)
	}
	var reason any
	if m.Reason != nil {
		reason = string(*m.Reason)
	}
	result, err := tx.tx.ExecContext(ctx, appendRunMilestoneSQL,
		m.RunID, m.Kind, invocation, terminal, outcome, reason,
		formatTime(m.RecordedAt))
	if err != nil {
		return fmt.Errorf("append run milestone %s %s: %w", m.RunID, m.Kind, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("append run milestone %s %s: %w", m.RunID, m.Kind, err)
	}
	if inserted == 0 {
		return nil
	}
	if err := tx.ClearRunHold(ctx, m.RunID); err != nil {
		return fmt.Errorf("append run milestone %s %s: %w", m.RunID, m.Kind, err)
	}
	return nil
}

// RecordInvocationObservation records the daemon's latest observation of one
// invocation, last write wins — including the run binding. The incoming
// value derives from the durable run and attempt records the engine just
// joined; the stored row is projection. Trusting the stored row enough to
// refuse the write would let a forged or corrupt projection row fail the
// reconcile pass that carries this write, which is exactly the authority the
// trust boundary denies it: a divergent stored binding is repaired by
// overwrite, never believed (the refute pass demonstrated the wedge).
func (tx *WriteTx) RecordInvocationObservation(
	ctx context.Context, o domain.InvocationObservation,
) error {
	if err := o.Validate(); err != nil {
		return fmt.Errorf("record invocation observation: %w", err)
	}
	live := 0
	if o.Live {
		live = 1
	}
	if _, err := tx.tx.ExecContext(ctx, upsertInvocationObservationSQL,
		o.InvocationID, o.RunID, o.Status, live, formatTime(o.ObservedAt)); err != nil {
		return fmt.Errorf("record invocation observation %s: %w", o.InvocationID, err)
	}
	return nil
}

// RecordRunHold records the run's current hold. The (reason, invocation)
// pair identifies the cause: re-observing the same cause advances only
// LastObservedAt and keeps the recorded first instant, while a changed cause
// replaces the row and restarts the span. The detail is not part of the
// cause: each write overwrites it with the latest. The stored row is
// projection, so
// it is only ever merged with, never trusted: a row the current vocabulary
// cannot express, an unparsable stored instant, or a stored span the
// incoming observation cannot extend (a stepped-back clock) all fall through
// to a plain overwrite instead of failing the workflow pass that carries
// this write.
func (tx *WriteTx) RecordRunHold(ctx context.Context, h domain.RunHoldObservation) error {
	if err := h.Validate(); err != nil {
		return fmt.Errorf("record run hold: %w", err)
	}
	var (
		storedReason, storedFirst string
		storedInvocation          sql.NullString
	)
	err := tx.tx.QueryRowContext(ctx, selectRunHoldMergeSQL, h.RunID).
		Scan(&storedReason, &storedInvocation, &storedFirst)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("record run hold %s: %w", h.RunID, err)
	case storedReason == string(h.Reason) &&
		storedInvocation.Valid == (h.InvocationID != nil) &&
		(!storedInvocation.Valid || storedInvocation.String == string(*h.InvocationID)):
		if firstAt, parseErr := parseTime(storedFirst); parseErr == nil &&
			!h.LastObservedAt.Before(firstAt) {
			h.FirstObservedAt = firstAt
		}
	}
	var invocation, check, agent, launchStage, launchDigest any
	if h.InvocationID != nil {
		invocation = string(*h.InvocationID)
	}
	if d := h.Detail; d != nil {
		check = string(d.Check)
		if d.Agent != nil {
			agent = *d.Agent
		}
		if d.LaunchStage != nil {
			launchStage, launchDigest = string(*d.LaunchStage), string(*d.LaunchDigest)
		}
	}
	var pending, refusal, item, task any
	if u := h.ImageUpkeep; u != nil {
		pending = string(u.Pending)
		if u.RefusalRecordID != nil {
			refusal = string(*u.RefusalRecordID)
		}
		if u.ProposalItemID != nil {
			item = string(*u.ProposalItemID)
		}
		if u.MaintenanceTaskID != nil {
			task = string(*u.MaintenanceTaskID)
		}
	}
	var usagePool any
	if h.UsagePool != nil {
		usagePool = string(*h.UsagePool)
	}
	var quotaPool, quotaWindow, quotaSource, quotaInvocation, quotaReadingAt, quotaResetsAt, quotaSetAt any
	var quotaAlternates any
	if q := h.QuotaWindow; q != nil {
		quotaPool, quotaWindow, quotaSource = string(q.Pool), q.Window, string(q.Source)
		quotaSetAt = formatTime(q.SetAt)
		if q.AlternateAgents != nil {
			body, err := json.Marshal(q.AlternateAgents)
			if err != nil {
				return fmt.Errorf("record run hold %s alternate agents: %w", h.RunID, err)
			}
			quotaAlternates = string(body)
		}
		if q.InvocationID != nil {
			quotaInvocation = string(*q.InvocationID)
		}
		if q.ReadingObservedAt != nil {
			quotaReadingAt = formatTime(*q.ReadingObservedAt)
		}
		if q.ResetsAt != nil {
			quotaResetsAt = formatTime(*q.ResetsAt)
		}
	}
	var capacityReservation, capacityReserved, capacityBudget, capacityAhead any
	if c := h.MachineCapacity; c != nil {
		capacityReservation, capacityReserved = c.ReservationMiB, c.ReservedMiB
		capacityBudget, capacityAhead = c.BudgetMiB, c.Ahead
	}
	if _, err := tx.tx.ExecContext(ctx, replaceRunHoldSQL,
		h.RunID, invocation, h.Reason, check, agent, launchStage, launchDigest,
		pending, refusal, item, task, usagePool,
		quotaPool, quotaWindow, quotaSource, quotaInvocation, quotaReadingAt, quotaResetsAt, quotaSetAt, quotaAlternates,
		capacityReservation, capacityReserved, capacityBudget, capacityAhead,
		formatTime(h.FirstObservedAt), formatTime(h.LastObservedAt)); err != nil {
		return fmt.Errorf("record run hold %s: %w", h.RunID, err)
	}
	return nil
}

// ClearRunHold removes the run's current hold observation, if any.
func (tx *WriteTx) ClearRunHold(ctx context.Context, runID domain.RunID) error {
	if runID == "" {
		return fmt.Errorf("clear run hold: %w", domain.ErrEmptyID)
	}
	if _, err := tx.tx.ExecContext(ctx, clearRunHoldSQL, runID); err != nil {
		return fmt.Errorf("clear run hold %s: %w", runID, err)
	}
	return nil
}

// ClearRunHoldCause removes the run's hold observation only while it still
// names the given cause. A lane that ends one cause must not erase a hold
// another cause is keeping (nor restart a live hold's span by deleting and
// re-recording it), so the cause is a delete predicate: the stored row is
// still never read back, and a row naming a different cause is left exactly
// as it stands.
func (tx *WriteTx) ClearRunHoldCause(
	ctx context.Context, runID domain.RunID, reason domain.RunHoldReason,
) error {
	if runID == "" {
		return fmt.Errorf("clear run hold cause: %w", domain.ErrEmptyID)
	}
	if !slices.Contains(domain.AllRunHoldReasons, reason) {
		return fmt.Errorf("clear run hold cause %s: reason %q: %w",
			runID, reason, domain.ErrInvalidRunHoldReason)
	}
	if _, err := tx.tx.ExecContext(ctx, clearRunHoldCauseSQL, runID, reason); err != nil {
		return fmt.Errorf("clear run hold cause %s: %w", runID, err)
	}
	return nil
}

// ListRunMilestones reconstructs the run's milestone timeline in append
// order. Every row re-validates; a milestone the current vocabulary cannot
// express fails the read closed instead of reconstructing as a thinner
// timeline.
func (tx *ReadTx) ListRunMilestones(
	ctx context.Context, runID domain.RunID,
) ([]domain.RunMilestone, error) {
	if runID == "" {
		return nil, fmt.Errorf("list run milestones: %w", domain.ErrEmptyID)
	}
	milestones, err := queryAll(ctx, tx.tx, func(row scanner) (domain.RunMilestone, error) {
		var (
			storedRun, kind, recordedAt          string
			invocation, terminal, outcome, cause sql.NullString
		)
		if err := row.Scan(&storedRun, &kind, &invocation, &terminal,
			&outcome, &cause, &recordedAt); err != nil {
			return domain.RunMilestone{}, err
		}
		at, err := parseTime(recordedAt)
		if err != nil {
			return domain.RunMilestone{}, fmt.Errorf("recorded_at %q: %w", recordedAt, err)
		}
		m := domain.RunMilestone{
			RunID:      domain.RunID(storedRun),
			Kind:       domain.RunMilestoneKind(kind),
			RecordedAt: at,
		}
		if invocation.Valid {
			id := domain.InvocationID(invocation.String)
			m.InvocationID = &id
		}
		if terminal.Valid {
			status := domain.ObservedInvocationStatus(terminal.String)
			m.Terminal = &status
		}
		if outcome.Valid {
			status := domain.ExecutionOutcomeStatus(outcome.String)
			m.Outcome = &status
		}
		if cause.Valid {
			reason := domain.RunHoldReason(cause.String)
			m.Reason = &reason
		}
		if err := m.Validate(); err != nil {
			return domain.RunMilestone{}, err
		}
		if m.RunID != runID {
			return domain.RunMilestone{}, fmt.Errorf("row names run %q: %w",
				m.RunID, domain.ErrParentKeyMismatch)
		}
		return m, nil
	}, listRunMilestonesSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list run milestones %s: %w", runID, err)
	}
	return milestones, nil
}

// ListInvocationObservations reconstructs the run's last invocation
// observations, re-validated, in invocation order.
func (tx *ReadTx) ListInvocationObservations(
	ctx context.Context, runID domain.RunID,
) ([]domain.InvocationObservation, error) {
	if runID == "" {
		return nil, fmt.Errorf("list invocation observations: %w", domain.ErrEmptyID)
	}
	observations, err := queryAll(ctx, tx.tx, func(row scanner) (domain.InvocationObservation, error) {
		o, err := scanInvocationObservation(row)
		if err != nil {
			return domain.InvocationObservation{}, err
		}
		if o.RunID != runID {
			return domain.InvocationObservation{}, fmt.Errorf("row names run %q: %w",
				o.RunID, domain.ErrParentKeyMismatch)
		}
		return o, nil
	}, listInvocationObservationsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list invocation observations %s: %w", runID, err)
	}
	return observations, nil
}

func scanInvocationObservation(row scanner) (domain.InvocationObservation, error) {
	var (
		invocation, run, status, observedAt string
		live                                int
	)
	if err := row.Scan(&invocation, &run, &status, &live, &observedAt); err != nil {
		return domain.InvocationObservation{}, err
	}
	at, err := parseTime(observedAt)
	if err != nil {
		return domain.InvocationObservation{},
			fmt.Errorf("observation %s observed_at %q: %w", invocation, observedAt, err)
	}
	o := domain.InvocationObservation{
		InvocationID: domain.InvocationID(invocation),
		RunID:        domain.RunID(run),
		Status:       domain.ObservedInvocationStatus(status),
		Live:         live != 0,
		ObservedAt:   at,
	}
	if err := o.Validate(); err != nil {
		return domain.InvocationObservation{}, err
	}
	return o, nil
}

// retiredAttendedMarkCheck is the admission check plan revision 97 removed
// with the attended first run. A hold recorded under it before the upgrade
// stays in run_hold_observations until the run's next hold, clear, or
// milestone, so GetRunHold reads it back with no detail, the shape the 0103
// contract gives a hold whose check it cannot name, rather than failing
// every run listing that includes the run.
const retiredAttendedMarkCheck = "agent_attended_mark"

// GetRunHold reconstructs the run's current hold, with presence reported
// separately because "not held" is the ordinary state, not an error (the
// LatestUnattendedOperationTransition shape).
func (tx *ReadTx) GetRunHold(
	ctx context.Context, runID domain.RunID,
) (domain.RunHoldObservation, bool, error) {
	if runID == "" {
		return domain.RunHoldObservation{}, false, fmt.Errorf("get run hold: %w", domain.ErrEmptyID)
	}
	var (
		storedRun, reason, first, last                string
		invocation, check, agent, stage, launchDigest sql.NullString
		pending, refusal, item, task, usagePool       sql.NullString
		quotaPool, quotaWindow, quotaSource           sql.NullString
		quotaInvocation, quotaReadingAt               sql.NullString
		quotaResetsAt, quotaSetAt, quotaAlternates    sql.NullString
		capacityReservation, capacityReserved         sql.NullInt64
		capacityBudget, capacityAhead                 sql.NullInt64
	)
	err := tx.tx.QueryRowContext(ctx, selectRunHoldSQL, runID).
		Scan(&storedRun, &invocation, &reason, &check, &agent, &stage, &launchDigest,
			&pending, &refusal, &item, &task, &usagePool,
			&quotaPool, &quotaWindow, &quotaSource, &quotaInvocation, &quotaReadingAt, &quotaResetsAt, &quotaSetAt,
			&quotaAlternates,
			&capacityReservation, &capacityReserved, &capacityBudget, &capacityAhead,
			&first, &last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.RunHoldObservation{}, false, nil
	case err != nil:
		return domain.RunHoldObservation{}, false, fmt.Errorf("get run hold %s: %w", runID, err)
	}
	firstAt, err := parseTime(first)
	if err != nil {
		return domain.RunHoldObservation{}, false,
			fmt.Errorf("get run hold %s first_observed_at %q: %w", runID, first, err)
	}
	lastAt, err := parseTime(last)
	if err != nil {
		return domain.RunHoldObservation{}, false,
			fmt.Errorf("get run hold %s last_observed_at %q: %w", runID, last, err)
	}
	h := domain.RunHoldObservation{
		RunID:           domain.RunID(storedRun),
		Reason:          domain.RunHoldReason(reason),
		FirstObservedAt: firstAt,
		LastObservedAt:  lastAt,
	}
	if invocation.Valid {
		id := domain.InvocationID(invocation.String)
		h.InvocationID = &id
	}
	switch {
	case check.Valid && check.String == retiredAttendedMarkCheck:
	case check.Valid:
		h.Detail = &domain.AdmissionRefusal{Check: domain.AdmissionCheck(check.String)}
		if agent.Valid {
			h.Detail.Agent = &agent.String
		}
		if stage.Valid {
			s := domain.StageName(stage.String)
			h.Detail.LaunchStage = &s
		}
		if launchDigest.Valid {
			d := domain.Digest(launchDigest.String)
			h.Detail.LaunchDigest = &d
		}
	case agent.Valid || stage.Valid || launchDigest.Valid:
		// A detail column without its check is not a detail the contract
		// can express; dropping it would hide the inconsistency.
		return domain.RunHoldObservation{}, false,
			fmt.Errorf("get run hold %s: detail without a check: %w", runID, domain.ErrHoldDetailMismatch)
	}
	switch {
	case pending.Valid:
		h.ImageUpkeep = &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPending(pending.String)}
		if refusal.Valid {
			id := domain.Digest(refusal.String)
			h.ImageUpkeep.RefusalRecordID = &id
		}
		if item.Valid {
			id := domain.ItemID(item.String)
			h.ImageUpkeep.ProposalItemID = &id
		}
		if task.Valid {
			id := domain.TaskID(task.String)
			h.ImageUpkeep.MaintenanceTaskID = &id
		}
	case refusal.Valid || item.Valid || task.Valid:
		// An upkeep reference without its pending kind is not a fact the
		// contract can express.
		return domain.RunHoldObservation{}, false,
			fmt.Errorf("get run hold %s: upkeep reference without a pending kind: %w",
				runID, domain.ErrHoldDetailMismatch)
	}
	if usagePool.Valid {
		pool := domain.UsagePoolID(usagePool.String)
		h.UsagePool = &pool
	}
	quota, err := readQuotaWindowHold(quotaPool, quotaWindow, quotaSource, quotaInvocation,
		quotaReadingAt, quotaResetsAt, quotaSetAt, quotaAlternates)
	if err != nil {
		return domain.RunHoldObservation{}, false, fmt.Errorf("get run hold %s: %w", runID, err)
	}
	h.QuotaWindow = quota
	capacity, err := readMachineCapacityHold(capacityReservation, capacityReserved, capacityBudget, capacityAhead)
	if err != nil {
		return domain.RunHoldObservation{}, false, fmt.Errorf("get run hold %s: %w", runID, err)
	}
	h.MachineCapacity = capacity
	if err := h.Validate(); err != nil {
		return domain.RunHoldObservation{}, false, fmt.Errorf("get run hold %s: %w", runID, err)
	}
	if h.RunID != runID {
		return domain.RunHoldObservation{}, false,
			fmt.Errorf("get run hold %s: row names run %q: %w", runID, h.RunID, domain.ErrParentKeyMismatch)
	}
	return h, true, nil
}

// readQuotaWindowHold rebuilds a quota window detail from its columns. The
// pool is the presence key: any other column without it is not a detail the
// contract can express. Validate on the whole hold checks the rest.
func readQuotaWindowHold(
	pool, window, source, invocation, readingAt, resetsAt, setAt, alternates sql.NullString,
) (*domain.QuotaWindowHold, error) {
	if !pool.Valid {
		if window.Valid || source.Valid || invocation.Valid || readingAt.Valid || resetsAt.Valid || setAt.Valid ||
			alternates.Valid {
			return nil, fmt.Errorf("quota window column without a pool: %w", domain.ErrHoldDetailMismatch)
		}
		return nil, nil
	}
	hold := &domain.QuotaWindowHold{
		Pool: domain.UsagePoolID(pool.String),
		ExhaustedMark: domain.ExhaustedMark{
			Window: window.String, Source: domain.ExhaustedMarkSource(source.String),
		},
	}
	if invocation.Valid {
		hold.InvocationID = new(domain.InvocationID(invocation.String))
	}
	for _, column := range []struct {
		name  string
		value sql.NullString
		into  **time.Time
	}{
		{"quota_reading_observed_at", readingAt, &hold.ReadingObservedAt},
		{"quota_resets_at", resetsAt, &hold.ResetsAt},
	} {
		if !column.value.Valid {
			continue
		}
		at, err := parseTime(column.value.String)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", column.name, column.value.String, err)
		}
		*column.into = &at
	}
	if setAt.Valid {
		at, err := parseTime(setAt.String)
		if err != nil {
			return nil, fmt.Errorf("quota_set_at %q: %w", setAt.String, err)
		}
		hold.SetAt = at
	}
	if alternates.Valid {
		// Validate on the whole hold checks the decoded offers; an empty or
		// unsorted list fails there as a corrupt row.
		if err := json.Unmarshal([]byte(alternates.String), &hold.AlternateAgents); err != nil {
			return nil, fmt.Errorf("quota_alternate_agents: %w", errRowInconsistent)
		}
	}
	return hold, nil
}

// readMachineCapacityHold rebuilds a machine capacity detail from its
// columns, which are all present or all absent. Validate on the whole hold
// checks the numbers against each other and the reason.
func readMachineCapacityHold(reservation, reserved, budget, ahead sql.NullInt64) (*domain.MachineCapacityHold, error) {
	columns := []sql.NullInt64{reservation, reserved, budget, ahead}
	present := 0
	for _, column := range columns {
		if column.Valid {
			present++
		}
	}
	switch present {
	case 0:
		return nil, nil
	case len(columns):
		return &domain.MachineCapacityHold{
			ReservationMiB: int(reservation.Int64), ReservedMiB: int(reserved.Int64),
			BudgetMiB: int(budget.Int64), Ahead: int(ahead.Int64),
		}, nil
	}
	return nil, fmt.Errorf("machine capacity columns partly present: %w", domain.ErrHoldDetailMismatch)
}

// ObserveRun composes the run's observation aggregate: the milestone
// timeline, the current hold, and the last invocation observations. This is
// the store read surface an operator client consumes (issue #394); it
// re-validates everything it returns and holds no authority over the
// workflow.
func (tx *ReadTx) ObserveRun(
	ctx context.Context, runID domain.RunID,
) (domain.RunObservation, error) {
	milestones, err := tx.ListRunMilestones(ctx, runID)
	if err != nil {
		return domain.RunObservation{}, fmt.Errorf("observe run: %w", err)
	}
	hold, found, err := tx.GetRunHold(ctx, runID)
	if err != nil {
		return domain.RunObservation{}, fmt.Errorf("observe run: %w", err)
	}
	observations, err := tx.ListInvocationObservations(ctx, runID)
	if err != nil {
		return domain.RunObservation{}, fmt.Errorf("observe run: %w", err)
	}
	observation := domain.RunObservation{
		RunID:       runID,
		Milestones:  milestones,
		Invocations: observations,
	}
	if found {
		observation.Hold = &hold
	}
	if err := observation.Validate(); err != nil {
		return domain.RunObservation{}, fmt.Errorf("observe run %s: %w", runID, err)
	}
	return observation, nil
}
