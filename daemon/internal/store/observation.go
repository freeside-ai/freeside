package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Run observation rows (migration 0024) are the operator-facing projection
// of run progress: written beside the workflow facts, never read by them.
// The write methods live on InternalTx because the rows are non-synchronized
// bookkeeping; the read surface re-validates every row and fails closed, so
// a row the current vocabulary cannot express is an error, never a silently
// weaker observation (issue #394; the store's reconstruction-gate
// convention).

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
			first_observed_at, last_observed_at
		FROM run_hold_observations WHERE run_id = ?`
	replaceRunHoldSQL = `INSERT INTO run_hold_observations
		(run_id, invocation_id, reason, first_observed_at, last_observed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (run_id) DO UPDATE SET
			invocation_id = excluded.invocation_id,
			reason = excluded.reason,
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
func (tx *InternalTx) AppendRunMilestone(ctx context.Context, m domain.RunMilestone) error {
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
func (tx *InternalTx) RecordInvocationObservation(
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
// replaces the row and restarts the span. The stored row is projection, so
// it is only ever merged with, never trusted: a row the current vocabulary
// cannot express, an unparsable stored instant, or a stored span the
// incoming observation cannot extend (a stepped-back clock) all fall through
// to a plain overwrite instead of failing the workflow pass that carries
// this write.
func (tx *InternalTx) RecordRunHold(ctx context.Context, h domain.RunHoldObservation) error {
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
	var invocation any
	if h.InvocationID != nil {
		invocation = string(*h.InvocationID)
	}
	if _, err := tx.tx.ExecContext(ctx, replaceRunHoldSQL,
		h.RunID, invocation, h.Reason,
		formatTime(h.FirstObservedAt), formatTime(h.LastObservedAt)); err != nil {
		return fmt.Errorf("record run hold %s: %w", h.RunID, err)
	}
	return nil
}

// ClearRunHold removes the run's current hold observation, if any.
func (tx *InternalTx) ClearRunHold(ctx context.Context, runID domain.RunID) error {
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
func (tx *InternalTx) ClearRunHoldCause(
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
	rows, err := tx.tx.QueryContext(ctx, listRunMilestonesSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list run milestones %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()
	var milestones []domain.RunMilestone
	for rows.Next() {
		var (
			storedRun, kind, recordedAt          string
			invocation, terminal, outcome, cause sql.NullString
		)
		if err := rows.Scan(&storedRun, &kind, &invocation, &terminal,
			&outcome, &cause, &recordedAt); err != nil {
			return nil, fmt.Errorf("list run milestones %s: %w", runID, err)
		}
		at, err := parseTime(recordedAt)
		if err != nil {
			return nil, fmt.Errorf("list run milestones %s recorded_at %q: %w",
				runID, recordedAt, err)
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
			return nil, fmt.Errorf("list run milestones %s: %w", runID, err)
		}
		if m.RunID != runID {
			return nil, fmt.Errorf("list run milestones %s: row names run %q: %w",
				runID, m.RunID, domain.ErrParentKeyMismatch)
		}
		milestones = append(milestones, m)
	}
	if err := rows.Err(); err != nil {
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
	rows, err := tx.tx.QueryContext(ctx, listInvocationObservationsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list invocation observations %s: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()
	var observations []domain.InvocationObservation
	for rows.Next() {
		o, err := scanInvocationObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("list invocation observations %s: %w", runID, err)
		}
		if o.RunID != runID {
			return nil, fmt.Errorf("list invocation observations %s: row names run %q: %w",
				runID, o.RunID, domain.ErrParentKeyMismatch)
		}
		observations = append(observations, o)
	}
	if err := rows.Err(); err != nil {
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
		storedRun, reason, first, last string
		invocation                     sql.NullString
	)
	err := tx.tx.QueryRowContext(ctx, selectRunHoldSQL, runID).
		Scan(&storedRun, &invocation, &reason, &first, &last)
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
	if err := h.Validate(); err != nil {
		return domain.RunHoldObservation{}, false, fmt.Errorf("get run hold %s: %w", runID, err)
	}
	if h.RunID != runID {
		return domain.RunHoldObservation{}, false,
			fmt.Errorf("get run hold %s: row names run %q: %w", runID, h.RunID, domain.ErrParentKeyMismatch)
	}
	return h, true, nil
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
