package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putReviewRecordSQL = `
INSERT INTO review_records
    (invocation_id, run_id, round, base_sha, head_sha, outcome, completed_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`

func reviewBodyDigest(body string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
}

func reviewBodyAuthority(body string) string {
	return reviewBodyDigest(body) + body
}

// PutReviewRecord atomically persists one completed pass and its immutable raw
// findings. Replays converge only when the complete byte-form agrees.
func (tx *WriteTx) PutReviewRecord(
	ctx context.Context, record domain.ReviewRecord, findings []domain.Finding,
) error {
	if err := record.Validate(); err != nil {
		return fmt.Errorf("put review record %q: %w", record.InvocationID, err)
	}
	if len(findings) != len(record.FindingIDs) {
		return fmt.Errorf("put review record %q finding count: %w",
			record.InvocationID, domain.ErrParentKeyMismatch)
	}
	findingsByID := make(map[domain.FindingID]domain.Finding, len(findings))
	for _, finding := range findings {
		if finding.RunID != record.RunID {
			return fmt.Errorf("put review record %q finding %q: %w",
				record.InvocationID, finding.ID, domain.ErrParentKeyMismatch)
		}
		if _, duplicate := findingsByID[finding.ID]; duplicate {
			return fmt.Errorf("put review record %q duplicate finding %q: %w",
				record.InvocationID, finding.ID, domain.ErrParentKeyMismatch)
		}
		findingsByID[finding.ID] = finding
	}
	for _, id := range record.FindingIDs {
		finding, ok := findingsByID[id]
		if !ok {
			return fmt.Errorf("put review record %q missing finding %q: %w",
				record.InvocationID, id, domain.ErrParentKeyMismatch)
		}
		if err := tx.PutFinding(ctx, finding); err != nil {
			return err
		}
	}
	if _, err := tx.GetReviewFailure(ctx, record.InvocationID); err == nil {
		return fmt.Errorf("put review record %q after failure: %w",
			record.InvocationID, ErrImmutableConflict)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	body, err := encode(record)
	if err != nil {
		return fmt.Errorf("put review record %q: %w", record.InvocationID, err)
	}
	err = tx.putImmutable(ctx, putReviewRecordSQL,
		[]any{
			record.InvocationID, record.RunID, record.Round, record.BaseSHA,
			record.HeadSHA, record.Outcome, record.CompletedAt.Format(time.RFC3339Nano),
			reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM review_records WHERE invocation_id = ?`,
		[]any{record.InvocationID}, reviewBodyAuthority(body))
	if err != nil {
		return fmt.Errorf("put review record %q: %w", record.InvocationID, err)
	}
	for i, id := range record.FindingIDs {
		if _, err := tx.tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO review_record_findings (invocation_id, finding_id, ordinal) VALUES (?, ?, ?)`,
			record.InvocationID, id, i); err != nil {
			return fmt.Errorf("put review record %q finding %q: %w", record.InvocationID, id, err)
		}
	}
	return nil
}

// GetReviewRecord reconstructs and cross-checks one completed pass.
func (tx *ReadTx) GetReviewRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ReviewRecord, error) {
	var runID, baseSHA, headSHA, outcome, completedAt, bodyDigest string
	var round int
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT run_id, round, base_sha, head_sha, outcome,
		completed_at, body_digest, body FROM review_records WHERE invocation_id = ?`, id).Scan(
		&runID, &round, &baseSHA, &headSHA, &outcome, &completedAt, &bodyDigest, &body)
	if err != nil {
		return domain.ReviewRecord{}, fmt.Errorf("get review record %q: %w", id, notFoundOr(err))
	}
	if bodyDigest != reviewBodyDigest(string(body)) {
		return domain.ReviewRecord{}, fmt.Errorf("get review record %q: %w", id, errRowInconsistent)
	}
	record, err := decode[domain.ReviewRecord](body)
	if err != nil {
		return domain.ReviewRecord{}, fmt.Errorf("get review record %q: %w", id, err)
	}
	if err := record.Validate(); err != nil {
		return domain.ReviewRecord{}, fmt.Errorf("get review record %q: %w", id, err)
	}
	if record.InvocationID != id || record.RunID != domain.RunID(runID) ||
		record.Round != round || record.BaseSHA != baseSHA || record.HeadSHA != headSHA ||
		string(record.Outcome) != outcome || record.CompletedAt.Format(time.RFC3339Nano) != completedAt {
		return domain.ReviewRecord{}, fmt.Errorf("get review record %q: %w", id, errRowInconsistent)
	}
	rows, err := tx.tx.QueryContext(ctx, `SELECT finding_id FROM review_record_findings
        WHERE invocation_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return domain.ReviewRecord{}, err
	}
	defer rows.Close() //nolint:errcheck // read-only query; iteration error checked below
	ids := make([]domain.FindingID, 0, len(record.FindingIDs))
	for rows.Next() {
		var findingID string
		if err := rows.Scan(&findingID); err != nil {
			return domain.ReviewRecord{}, err
		}
		ids = append(ids, domain.FindingID(findingID))
	}
	if err := rows.Err(); err != nil {
		return domain.ReviewRecord{}, err
	}
	if !slices.Equal(ids, record.FindingIDs) {
		return domain.ReviewRecord{}, fmt.Errorf("get review record %q findings: %w", id, errRowInconsistent)
	}
	return record, nil
}

// LatestReviewRecord returns the highest recorded round for one run.
func (tx *ReadTx) LatestReviewRecord(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewRecord, error) {
	var id string
	err := tx.tx.QueryRowContext(ctx, `SELECT invocation_id FROM review_records
        WHERE run_id = ? ORDER BY round DESC LIMIT 1`, runID).Scan(&id)
	if err != nil {
		return domain.ReviewRecord{}, fmt.Errorf("latest review record %q: %w", runID, notFoundOr(err))
	}
	return tx.GetReviewRecord(ctx, domain.InvocationID(id))
}

// LatestReviewFailure returns the highest failed round for one run.
func (tx *ReadTx) LatestReviewFailure(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewFailure, error) {
	var id string
	err := tx.tx.QueryRowContext(ctx, `SELECT invocation_id FROM review_failures
        WHERE run_id = ? ORDER BY round DESC LIMIT 1`, runID).Scan(&id)
	if err != nil {
		return domain.ReviewFailure{}, fmt.Errorf("latest review failure %q: %w", runID, notFoundOr(err))
	}
	return tx.GetReviewFailure(ctx, domain.InvocationID(id))
}

const putReviewFailureSQL = `
INSERT INTO review_failures (invocation_id, run_id, round, failure_class, observed_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`

func (tx *WriteTx) PutReviewFailure(ctx context.Context, failure domain.ReviewFailure) error {
	if err := failure.Validate(); err != nil {
		return fmt.Errorf("put review failure %q: %w", failure.InvocationID, err)
	}
	if _, err := tx.GetReviewRecord(ctx, failure.InvocationID); err == nil {
		return fmt.Errorf("put review failure %q after result: %w",
			failure.InvocationID, ErrImmutableConflict)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	body, err := encode(failure)
	if err != nil {
		return fmt.Errorf("put review failure %q: %w", failure.InvocationID, err)
	}
	if err := tx.putImmutable(ctx, putReviewFailureSQL,
		[]any{
			failure.InvocationID, failure.RunID, failure.Round, failure.Class,
			failure.ObservedAt.Format(time.RFC3339Nano), reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM review_failures WHERE invocation_id = ?`,
		[]any{failure.InvocationID}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put review failure %q: %w", failure.InvocationID, err)
	}
	return nil
}

func (tx *ReadTx) GetReviewFailure(
	ctx context.Context, id domain.InvocationID,
) (domain.ReviewFailure, error) {
	var runID, class, observedAt, bodyDigest string
	var round int
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT run_id, round, failure_class, observed_at, body_digest, body
        FROM review_failures WHERE invocation_id = ?`, id).Scan(
		&runID, &round, &class, &observedAt, &bodyDigest, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReviewFailure{}, fmt.Errorf("get review failure %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return domain.ReviewFailure{}, fmt.Errorf("get review failure %q: %w", id, err)
	}
	if bodyDigest != reviewBodyDigest(string(body)) {
		return domain.ReviewFailure{}, fmt.Errorf("get review failure %q: %w", id, errRowInconsistent)
	}
	failure, err := decode[domain.ReviewFailure](body)
	if err != nil {
		return domain.ReviewFailure{}, fmt.Errorf("get review failure %q: %w", id, err)
	}
	if err := failure.Validate(); err != nil {
		return domain.ReviewFailure{}, fmt.Errorf("get review failure %q: %w", id, err)
	}
	if failure.InvocationID != id || failure.RunID != domain.RunID(runID) ||
		failure.Round != round || string(failure.Class) != class ||
		failure.ObservedAt.Format(time.RFC3339Nano) != observedAt {
		return domain.ReviewFailure{}, fmt.Errorf("get review failure %q: %w", id, errRowInconsistent)
	}
	return failure, nil
}
