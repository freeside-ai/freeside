package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Run hold spans (migration 0145, issue #3235) are the history of a run's
// holds. A closed span is written only when a hold ends: RecordRunHold
// replacing it with another cause, or ClearRunHold, ClearRunHoldCause, and
// ClearUpdateDrainHolds deleting it (AppendRunMilestone clears through
// ClearRunHold). The current
// hold stays in run_hold_observations alone, and the reads return it as the
// open span, so a hold is recorded once whenever it ends, including one that
// was open before the table existed.

const (
	selectEndingRunHoldsSQL = `SELECT run_id, reason, invocation_id,
			first_observed_at, last_observed_at
		FROM run_hold_observations WHERE `
	selectEndingRunHoldSQL         = selectEndingRunHoldsSQL + `run_id = ?`
	selectEndingRunHoldOfReasonSQL = selectEndingRunHoldsSQL + `run_id = ? AND reason = ?`
	selectEndingHoldsOfReasonSQL   = selectEndingRunHoldsSQL + `reason = ? ORDER BY run_id`
	insertRunHoldSpanSQL           = `INSERT INTO run_hold_spans
		(run_id, invocation_id, reason, first_observed_at, last_observed_at, cleared_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	listRunHoldSpansSQL = `SELECT run_id, invocation_id, reason,
			first_observed_at, last_observed_at, cleared_at
		FROM run_hold_spans WHERE run_id = ? ORDER BY id`
	listOpenRunHoldSpanSQL = `SELECT run_id, invocation_id, reason,
			first_observed_at, last_observed_at, NULL
		FROM run_hold_observations WHERE run_id = ?`
	listAllRunHoldSpansSQL = `SELECT run_id, invocation_id, reason,
			first_observed_at, last_observed_at, cleared_at
		FROM run_hold_spans ORDER BY id`
	listAllOpenRunHoldSpansSQL = `SELECT run_id, invocation_id, reason,
			first_observed_at, last_observed_at, NULL
		FROM run_hold_observations ORDER BY run_id`
)

// endedRunHold is the cause and instants of a current hold row that is
// ending, as stored. It is read only to copy it into the history.
type endedRunHold struct {
	runID               domain.RunID
	reason, first, last string
	invocation          sql.NullString
}

// recordRunHoldSpan writes the span of an ending hold, cleared at clearedAt.
// A clear time before the hold's last observation is raised to it, so a span
// never reads backwards. The stored current row is display state: when it
// can't make a valid span (an instant that doesn't parse, a reason the
// vocabulary no longer has), no span is written and the caller's clear or
// replacement goes ahead, as RecordRunHold's merge does. A database error on
// the insert still fails the transaction.
func (tx *WriteTx) recordRunHoldSpan(
	ctx context.Context, ended endedRunHold, clearedAt time.Time,
) error {
	first, firstErr := parseTime(ended.first)
	last, lastErr := parseTime(ended.last)
	if firstErr != nil || lastErr != nil {
		return nil
	}
	clearedAt = clearedAt.UTC()
	if clearedAt.Before(last) {
		clearedAt = last
	}
	span := domain.RunHoldSpan{
		RunID:           ended.runID,
		Reason:          domain.RunHoldReason(ended.reason),
		FirstObservedAt: first,
		LastObservedAt:  last,
		ClearedAt:       &clearedAt,
	}
	if ended.invocation.Valid {
		id := domain.InvocationID(ended.invocation.String)
		span.InvocationID = &id
	}
	if span.Validate() != nil {
		return nil
	}
	if _, err := tx.tx.ExecContext(ctx, insertRunHoldSpanSQL,
		span.RunID, ended.invocation, span.Reason,
		formatTime(first), formatTime(last), formatTime(clearedAt)); err != nil {
		return fmt.Errorf("record run hold span %s: %w", ended.runID, err)
	}
	return nil
}

// recordEndedRunHolds reads the current hold rows that query selects, the
// rows the caller is about to delete, and writes each one's span cleared at
// clearedAt.
func (tx *WriteTx) recordEndedRunHolds(
	ctx context.Context, clearedAt time.Time, query string, args ...any,
) error {
	ended, err := queryAll(ctx, tx.tx, func(row scanner) (endedRunHold, error) {
		var h endedRunHold
		err := row.Scan(&h.runID, &h.reason, &h.invocation, &h.first, &h.last)
		return h, err
	}, query, args...)
	if err != nil {
		return fmt.Errorf("read ending run holds: %w", err)
	}
	for _, h := range ended {
		if err := tx.recordRunHoldSpan(ctx, h, clearedAt); err != nil {
			return err
		}
	}
	return nil
}

// ListRunHoldSpans returns the run's holds in the order they happened: its
// closed spans, then its current hold, if any, as an open span. Every row
// re-validates, and a row that doesn't fails the read.
func (tx *ReadTx) ListRunHoldSpans(
	ctx context.Context, runID domain.RunID,
) ([]domain.RunHoldSpan, error) {
	if runID == "" {
		return nil, fmt.Errorf("list run hold spans: %w", domain.ErrEmptyID)
	}
	scan := func(row scanner) (domain.RunHoldSpan, error) {
		s, err := scanRunHoldSpan(row)
		if err != nil {
			return domain.RunHoldSpan{}, err
		}
		if s.RunID != runID {
			return domain.RunHoldSpan{}, fmt.Errorf("row names run %q: %w",
				s.RunID, domain.ErrParentKeyMismatch)
		}
		return s, nil
	}
	closed, err := queryAll(ctx, tx.tx, scan, listRunHoldSpansSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list run hold spans %s: %w", runID, err)
	}
	open, err := queryAll(ctx, tx.tx, scan, listOpenRunHoldSpanSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list run hold spans %s current hold: %w", runID, err)
	}
	return append(closed, open...), nil
}

// ListRunHoldSpansBetween returns every run's holds that overlap [from, to):
// a span that started before to and is still open or was cleared after from.
// They are ordered by first observation, then run, then the order they were
// recorded, with each run's current hold after its closed spans. The filter
// and sort run on parsed instants, because stored RFC3339Nano text doesn't
// sort within a second. Every row re-validates, and a row that doesn't fails
// the read.
func (tx *ReadTx) ListRunHoldSpansBetween(
	ctx context.Context, from, to time.Time,
) ([]domain.RunHoldSpan, error) {
	if !from.Before(to) {
		return nil, fmt.Errorf("list run hold spans between %s and %s: %w",
			formatTime(from), formatTime(to), domain.ErrTimestampOutOfOrder)
	}
	closed, err := queryAll(ctx, tx.tx, scanRunHoldSpan, listAllRunHoldSpansSQL)
	if err != nil {
		return nil, fmt.Errorf("list run hold spans: %w", err)
	}
	open, err := queryAll(ctx, tx.tx, scanRunHoldSpan, listAllOpenRunHoldSpansSQL)
	if err != nil {
		return nil, fmt.Errorf("list run hold spans current holds: %w", err)
	}
	var spans []domain.RunHoldSpan
	for _, s := range append(closed, open...) {
		if s.FirstObservedAt.Before(to) && (s.ClearedAt == nil || s.ClearedAt.After(from)) {
			spans = append(spans, s)
		}
	}
	slices.SortStableFunc(spans, func(a, b domain.RunHoldSpan) int {
		return cmp.Or(a.FirstObservedAt.Compare(b.FirstObservedAt), cmp.Compare(a.RunID, b.RunID))
	})
	return spans, nil
}

func scanRunHoldSpan(row scanner) (domain.RunHoldSpan, error) {
	var (
		run, reason, first, last string
		invocation, cleared      sql.NullString
	)
	if err := row.Scan(&run, &invocation, &reason, &first, &last, &cleared); err != nil {
		return domain.RunHoldSpan{}, err
	}
	s := domain.RunHoldSpan{RunID: domain.RunID(run), Reason: domain.RunHoldReason(reason)}
	var err error
	if s.FirstObservedAt, err = parseTime(first); err != nil {
		return domain.RunHoldSpan{}, fmt.Errorf("run hold span %s first_observed_at: %w", run, err)
	}
	if s.LastObservedAt, err = parseTime(last); err != nil {
		return domain.RunHoldSpan{}, fmt.Errorf("run hold span %s last_observed_at: %w", run, err)
	}
	if cleared.Valid {
		at, err := parseTime(cleared.String)
		if err != nil {
			return domain.RunHoldSpan{}, fmt.Errorf("run hold span %s cleared_at: %w", run, err)
		}
		s.ClearedAt = &at
	}
	if invocation.Valid {
		id := domain.InvocationID(invocation.String)
		s.InvocationID = &id
	}
	if err := s.Validate(); err != nil {
		return domain.RunHoldSpan{}, err
	}
	return s, nil
}
