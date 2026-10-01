package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putDriftAuditSQL = `
INSERT INTO drift_audits (run_id, round, content_digest, body_digest, body)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (run_id, round) DO NOTHING`

// validateDriftAuditBinding re-runs every authoritative join instead of
// trusting the artifact's copied values (plan §7 Review Drift). The audited
// round must be a recorded round with findings, and the artifact's base and
// head must be that round's. The specification and policy digests must be the
// run's. Every reversal must name a finding that one of the run's review
// records lists at or before the audited round, so the answer never changes as
// later rounds are recorded. Whether a named finding's fix may be reversed is
// the route gate's check (#1051), not this one.
func (tx *ReadTx) validateDriftAuditBinding(ctx context.Context, audit domain.DriftAudit) error {
	record, err := tx.reviewRecordForRound(ctx, audit.RunID, audit.Round)
	if err != nil {
		return err
	}
	if record.Outcome != domain.ReviewFindings ||
		record.BaseSHA != audit.BaseSHA || record.HeadSHA != audit.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	// The run's specification and policy digests are fixed at creation and are
	// the authority; the artifact's copies are not (the finding-adjudication
	// rule).
	run, err := tx.GetRun(ctx, audit.RunID)
	if err != nil {
		return err
	}
	if audit.ApprovedSpecDigest != run.SpecDigest || audit.ResolvedPolicyDigest != run.PolicyDigest {
		return domain.ErrParentKeyMismatch
	}
	if len(audit.Reversals) == 0 {
		return nil
	}
	// GetReviewRecord cross-checks each row's copied run and round against its
	// body, so a row this keyed query selects is the run's. A corrupted key can
	// only hide a row, which fails the reversal check closed.
	rows, err := tx.tx.QueryContext(ctx, `SELECT invocation_id FROM review_records
		WHERE run_id = ? AND round <= ? ORDER BY round`, audit.RunID, audit.Round)
	if err != nil {
		return err
	}
	var ids []domain.InvocationID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, domain.InvocationID(id))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	listed := make(map[domain.FindingID]struct{})
	for _, id := range ids {
		earlier, err := tx.GetReviewRecord(ctx, id)
		if err != nil {
			return err
		}
		if earlier.RunID != audit.RunID || earlier.Round > audit.Round {
			return errRowInconsistent
		}
		for _, findingID := range earlier.FindingIDs {
			listed[findingID] = struct{}{}
		}
	}
	for _, reversal := range audit.Reversals {
		if _, ok := listed[reversal.FindingID]; !ok {
			return fmt.Errorf("reversal names finding %q outside the run's review records: %w",
				reversal.FindingID, domain.ErrParentKeyMismatch)
		}
	}
	return nil
}

// PutDriftAudit records one review round's drift audit. A byte-identical
// replay converges; a different audit for the same round is an immutable
// conflict.
//
// A round's verdict enters yield history once a later round is recorded, and
// an item's stored yield history must equal the history re-derived from these
// rows. An audit that appears for a round that already has a successor,
// recorded or failed, would change histories items already snapshotted, so it
// is refused here. The latest round is always writable: no history shows its
// own last round's verdict, so that row can't change a snapshot however late
// it is written.
func (tx *WriteTx) PutDriftAudit(ctx context.Context, audit domain.DriftAudit) error {
	body, err := encode(audit)
	if err != nil {
		return fmt.Errorf("put drift audit %q round %d: %w", audit.RunID, audit.Round, err)
	}
	// Decode what is about to be stored through the read path's decoder, so a
	// body the reads would refuse (over the size bound) is never written.
	if _, err := domain.DecodeDriftAudit([]byte(body)); err != nil {
		return fmt.Errorf("put drift audit %q round %d: %w", audit.RunID, audit.Round, err)
	}
	if err := tx.validateDriftAuditBinding(ctx, audit); err != nil {
		return fmt.Errorf("put drift audit %q round %d binding: %w", audit.RunID, audit.Round, err)
	}
	// Reconstruct an existing row before the replay comparison, so corruption
	// in a copied lookup column cannot hide behind an unchanged body.
	if _, err := tx.GetDriftAuditForRound(ctx, audit.RunID, audit.Round); errors.Is(err, ErrNotFound) {
		var successors int
		// Failed rounds share the run's round numbers, so a later failure
		// proves the run moved past this round as a later record does.
		if err := tx.tx.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM review_records WHERE run_id = ? AND round > ?) +
			(SELECT COUNT(*) FROM review_failures WHERE run_id = ? AND round > ?)`,
			audit.RunID, audit.Round, audit.RunID, audit.Round).Scan(&successors); err != nil {
			return fmt.Errorf("put drift audit %q round %d: %w", audit.RunID, audit.Round, err)
		}
		if successors > 0 {
			return fmt.Errorf("put drift audit %q round %d after a later round: %w",
				audit.RunID, audit.Round, ErrImmutableConflict)
		}
	} else if err != nil {
		return fmt.Errorf("put drift audit %q round %d existing row: %w", audit.RunID, audit.Round, err)
	}
	if err := tx.putImmutable(ctx, putDriftAuditSQL,
		[]any{audit.RunID, audit.Round, audit.Digest, reviewBodyDigest(body), body},
		`SELECT body_digest || body FROM drift_audits WHERE run_id = ? AND round = ?`,
		[]any{audit.RunID, audit.Round}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put drift audit %q round %d: %w", audit.RunID, audit.Round, err)
	}
	return nil
}

type driftAuditRow struct {
	runID         string
	round         int
	contentDigest string
	bodyDigest    string
	body          []byte
}

const selectDriftAuditColumns = `run_id, round, content_digest, body_digest, body`

func scanDriftAuditRow(sc scanner) (driftAuditRow, error) {
	var row driftAuditRow
	err := sc.Scan(&row.runID, &row.round, &row.contentDigest, &row.bodyDigest, &row.body)
	return row, err
}

// reconstructDriftAudit is the one rebuild path for every read. It re-runs the
// body integrity digest, the strict decode with the artifact's validation
// (which recomputes the content digest and the verdict's reversal
// cardinality), the agreement of the copied keys with the decoded body, and
// the binding to the run's records. No copied column and no stored commit,
// digest, or finding name is trusted.
func (tx *ReadTx) reconstructDriftAudit(ctx context.Context, row driftAuditRow) (domain.DriftAudit, error) {
	if row.bodyDigest != reviewBodyDigest(string(row.body)) {
		return domain.DriftAudit{}, errRowInconsistent
	}
	audit, err := domain.DecodeDriftAudit(row.body)
	if err != nil {
		return domain.DriftAudit{}, fmt.Errorf("stored row invalid: %w", err)
	}
	if string(audit.RunID) != row.runID || audit.Round != row.round ||
		string(audit.Digest) != row.contentDigest {
		return domain.DriftAudit{}, errRowInconsistent
	}
	if err := tx.validateDriftAuditBinding(ctx, audit); err != nil {
		return domain.DriftAudit{}, err
	}
	return audit, nil
}

// GetDriftAudit reconstructs one audit by its content digest.
func (tx *ReadTx) GetDriftAudit(ctx context.Context, digest domain.Digest) (domain.DriftAudit, error) {
	row, err := scanDriftAuditRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectDriftAuditColumns+` FROM drift_audits WHERE content_digest = ?`, digest))
	if err != nil {
		return domain.DriftAudit{}, fmt.Errorf("get drift audit %q: %w", digest, notFoundOr(err))
	}
	audit, err := tx.reconstructDriftAudit(ctx, row)
	if err != nil {
		return domain.DriftAudit{}, fmt.Errorf("get drift audit %q: %w", digest, err)
	}
	return audit, nil
}

// GetDriftAuditForRound reconstructs one round's audit. ErrNotFound means no
// audit was recorded for the round.
func (tx *ReadTx) GetDriftAuditForRound(
	ctx context.Context, runID domain.RunID, round int,
) (domain.DriftAudit, error) {
	row, err := scanDriftAuditRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectDriftAuditColumns+` FROM drift_audits WHERE run_id = ? AND round = ?`,
		runID, round))
	if err != nil {
		return domain.DriftAudit{}, fmt.Errorf("get drift audit %q round %d: %w", runID, round, notFoundOr(err))
	}
	audit, err := tx.reconstructDriftAudit(ctx, row)
	if err != nil {
		return domain.DriftAudit{}, fmt.Errorf("get drift audit %q round %d: %w", runID, round, err)
	}
	return audit, nil
}

// ListDriftAudits returns one run's audits in round order. It enumerates the
// whole table and reconstructs every row before the run filter, so a corrupted
// copied run key cannot move a row out of all keyed reads and make an audited
// round look unaudited (the review-record list pattern).
func (tx *ReadTx) ListDriftAudits(ctx context.Context, runID domain.RunID) ([]domain.DriftAudit, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT `+selectDriftAuditColumns+` FROM drift_audits ORDER BY run_id, round`)
	if err != nil {
		return nil, fmt.Errorf("list drift audits %q: %w", runID, err)
	}
	var raw []driftAuditRow
	for rows.Next() {
		row, err := scanDriftAuditRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list drift audits %q row %d: %w", runID, len(raw)+1, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list drift audits %q: %w", runID, err)
	}
	out := make([]domain.DriftAudit, 0, len(raw))
	for i, row := range raw {
		audit, err := tx.reconstructDriftAudit(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("list drift audits %q row %d: %w", runID, i+1, err)
		}
		if audit.RunID == runID {
			out = append(out, audit)
		}
	}
	return out, nil
}
