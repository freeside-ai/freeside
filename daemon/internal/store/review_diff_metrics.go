package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// reviewRoundDiffMetricsRecord is the stored body of one review round's diff
// metrics. The round keys live in the body so reconstruction can cross-check
// the copied lookup columns instead of trusting them.
type reviewRoundDiffMetricsRecord struct {
	RunID   domain.RunID                  `json:"run_id"`
	Round   int                           `json:"round"`
	Metrics domain.ReviewRoundDiffMetrics `json:"metrics"`
}

func (r reviewRoundDiffMetricsRecord) Validate() error {
	if r.RunID == "" {
		return fmt.Errorf("review round diff metrics run_id: %w", domain.ErrEmptyID)
	}
	if r.Round < 1 {
		return fmt.Errorf("review round diff metrics round %d: %w", r.Round, domain.ErrNonPositive)
	}
	return r.Metrics.Validate()
}

const putReviewRoundDiffMetricsSQL = `
INSERT INTO review_round_diff_metrics (run_id, round, body_digest, body)
VALUES (?, ?, ?, ?)
ON CONFLICT (run_id, round) DO NOTHING`

// checkReviewRoundDiffMetrics is the single definition of what a round's
// metrics must name (plan §7 Review Drift). The cumulative pair runs from the
// round's bound base to its candidate head. The round pair starts at the
// previous recorded round's candidate head, or at the bound base when the run
// has no earlier round. previous is nil for that first round.
func checkReviewRoundDiffMetrics(
	metrics domain.ReviewRoundDiffMetrics, record domain.ReviewRecord, previous *domain.ReviewRecord,
) error {
	roundBase := record.BaseSHA
	if previous != nil {
		roundBase = previous.HeadSHA
	}
	if metrics.Cumulative.BaseSHA != record.BaseSHA || metrics.Cumulative.HeadSHA != record.HeadSHA ||
		metrics.Round.BaseSHA != roundBase || metrics.Round.HeadSHA != record.HeadSHA {
		return fmt.Errorf("round %d diff metrics name other commits than its review records: %w",
			record.Round, domain.ErrParentKeyMismatch)
	}
	return nil
}

// validateReviewRoundDiffMetricsBinding re-reads the review records the
// metrics describe instead of trusting the commits the row names: the round
// must exist, and both pairs must name that round's and its predecessor's
// recorded commits.
func (tx *ReadTx) validateReviewRoundDiffMetricsBinding(
	ctx context.Context, record reviewRoundDiffMetricsRecord,
) error {
	review, err := tx.reviewRecordForRound(ctx, record.RunID, record.Round)
	if err != nil {
		return err
	}
	var previous *domain.ReviewRecord
	var previousID string
	err = tx.tx.QueryRowContext(ctx, `SELECT invocation_id FROM review_records
		WHERE run_id = ? AND round < ? ORDER BY round DESC LIMIT 1`,
		record.RunID, record.Round).Scan(&previousID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		prior, err := tx.GetReviewRecord(ctx, domain.InvocationID(previousID))
		if err != nil {
			return err
		}
		previous = &prior
	}
	return checkReviewRoundDiffMetrics(record.Metrics, review, previous)
}

// PutReviewRoundDiffMetrics records one review round's diff metrics. A
// byte-identical replay converges; different metrics for the same round are an
// immutable conflict.
//
// Rows are written when the round is recorded and never backfilled. An item's
// stored yield history must equal the history re-derived from these rows, so
// metrics that appear after an item snapshotted the round would stop that item
// from loading. A round that already has a successor, recorded or failed, is
// therefore refused here. The caller owns the remaining ordering: write the
// row in the transaction that records the round, before any item reads its
// history.
func (tx *WriteTx) PutReviewRoundDiffMetrics(
	ctx context.Context, runID domain.RunID, round int, metrics domain.ReviewRoundDiffMetrics,
) error {
	record := reviewRoundDiffMetricsRecord{RunID: runID, Round: round, Metrics: metrics}
	body, err := encode(record)
	if err != nil {
		return fmt.Errorf("put review round diff metrics %q round %d: %w", runID, round, err)
	}
	if err := tx.validateReviewRoundDiffMetricsBinding(ctx, record); err != nil {
		return fmt.Errorf("put review round diff metrics %q round %d binding: %w", runID, round, err)
	}
	// Reconstruct an existing row before the replay comparison, so corruption
	// in a copied lookup column cannot hide behind an unchanged body.
	if _, err := tx.GetReviewRoundDiffMetrics(ctx, runID, round); errors.Is(err, ErrNotFound) {
		var successors int
		// Failed rounds share the run's round numbers, so a later failure
		// proves the run moved past this round as a later record does.
		if err := tx.tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM review_records WHERE run_id = ? AND round > ?) +
			(SELECT COUNT(*) FROM review_failures WHERE run_id = ? AND round > ?)`,
			runID, round, runID, round).Scan(&successors); err != nil {
			return fmt.Errorf("put review round diff metrics %q round %d: %w", runID, round, err)
		}
		if successors > 0 {
			return fmt.Errorf("put review round diff metrics %q round %d after a later round: %w",
				runID, round, ErrImmutableConflict)
		}
	} else if err != nil {
		return fmt.Errorf("put review round diff metrics %q round %d existing row: %w", runID, round, err)
	}
	if err := tx.putImmutable(ctx, putReviewRoundDiffMetricsSQL,
		[]any{runID, round, reviewBodyDigest(body), body},
		`SELECT body_digest || body FROM review_round_diff_metrics WHERE run_id = ? AND round = ?`,
		[]any{runID, round}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put review round diff metrics %q round %d: %w", runID, round, err)
	}
	return nil
}

type reviewRoundDiffMetricsRow struct {
	runID      string
	round      int
	bodyDigest string
	body       []byte
}

func scanReviewRoundDiffMetricsRow(sc scanner) (reviewRoundDiffMetricsRow, error) {
	var row reviewRoundDiffMetricsRow
	err := sc.Scan(&row.runID, &row.round, &row.bodyDigest, &row.body)
	return row, err
}

// reconstructReviewRoundDiffMetrics is the one rebuild path for every read. It
// re-runs the body integrity digest, the validation backstop, the agreement of
// the copied keys with the decoded body, and the binding to the review
// records. No copied column and no stored commit name is trusted.
func (tx *ReadTx) reconstructReviewRoundDiffMetrics(
	ctx context.Context, row reviewRoundDiffMetricsRow,
) (reviewRoundDiffMetricsRecord, error) {
	if row.bodyDigest != reviewBodyDigest(string(row.body)) {
		return reviewRoundDiffMetricsRecord{}, errRowInconsistent
	}
	record, err := decode[reviewRoundDiffMetricsRecord](row.body)
	if err != nil {
		return reviewRoundDiffMetricsRecord{}, err
	}
	if string(record.RunID) != row.runID || record.Round != row.round {
		return reviewRoundDiffMetricsRecord{}, errRowInconsistent
	}
	if err := tx.validateReviewRoundDiffMetricsBinding(ctx, record); err != nil {
		return reviewRoundDiffMetricsRecord{}, err
	}
	return record, nil
}

// GetReviewRoundDiffMetrics reconstructs one round's metrics. ErrNotFound
// means nothing was recorded for the round.
func (tx *ReadTx) GetReviewRoundDiffMetrics(
	ctx context.Context, runID domain.RunID, round int,
) (domain.ReviewRoundDiffMetrics, error) {
	row, err := scanReviewRoundDiffMetricsRow(tx.tx.QueryRowContext(ctx,
		`SELECT run_id, round, body_digest, body FROM review_round_diff_metrics
		WHERE run_id = ? AND round = ?`, runID, round))
	if err != nil {
		return domain.ReviewRoundDiffMetrics{}, fmt.Errorf(
			"get review round diff metrics %q round %d: %w", runID, round, notFoundOr(err))
	}
	record, err := tx.reconstructReviewRoundDiffMetrics(ctx, row)
	if err != nil {
		return domain.ReviewRoundDiffMetrics{}, fmt.Errorf(
			"get review round diff metrics %q round %d: %w", runID, round, err)
	}
	return record.Metrics, nil
}

// ListReviewRoundDiffMetrics returns one run's recorded metrics keyed by
// round. It enumerates the whole table and reconstructs every row before the
// run filter, so a corrupted copied run key cannot move a row out of all keyed
// reads and make a recorded round look like a gap (the review-record list
// pattern).
func (tx *ReadTx) ListReviewRoundDiffMetrics(
	ctx context.Context, runID domain.RunID,
) (map[int]domain.ReviewRoundDiffMetrics, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT run_id, round, body_digest, body
		FROM review_round_diff_metrics ORDER BY run_id, round`)
	if err != nil {
		return nil, fmt.Errorf("list review round diff metrics %q: %w", runID, err)
	}
	var raw []reviewRoundDiffMetricsRow
	for rows.Next() {
		row, err := scanReviewRoundDiffMetricsRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list review round diff metrics %q row %d: %w", runID, len(raw)+1, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list review round diff metrics %q: %w", runID, err)
	}
	out := make(map[int]domain.ReviewRoundDiffMetrics)
	for i, row := range raw {
		record, err := tx.reconstructReviewRoundDiffMetrics(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("list review round diff metrics %q row %d: %w", runID, i+1, err)
		}
		if record.RunID == runID {
			out[record.Round] = record.Metrics
		}
	}
	return out, nil
}
