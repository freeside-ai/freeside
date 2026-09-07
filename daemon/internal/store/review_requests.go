package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PutReviewRequest is immutable, including the original request timestamp.
// Callers resuming an invocation must reuse the first record.
func (tx *WriteTx) PutReviewRequest(ctx context.Context, r domain.ReviewRequestRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := tx.ensureInvocationNotShadowRecorded(ctx, r.InvocationID); err != nil {
		return err
	}
	body, err := encode(r)
	if err != nil {
		return err
	}
	return tx.putImmutable(ctx, `INSERT INTO review_requests
		(invocation_id, run_id, round, base_sha, head_sha, requested_at, body_digest, body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (invocation_id) DO NOTHING`,
		[]any{r.InvocationID, r.RunID, r.Round, r.BaseSHA, r.HeadSHA, formatTime(r.RequestedAt), reviewBodyDigest(body), body},
		`SELECT body_digest || body FROM review_requests WHERE invocation_id = ?`,
		[]any{r.InvocationID}, reviewBodyAuthority(body))
}

func (tx *ReadTx) GetReviewRequest(ctx context.Context, id domain.InvocationID) (domain.ReviewRequestRecord, error) {
	var runID, base, head, requested, digest string
	var round int
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT run_id, round, base_sha, head_sha, requested_at, body_digest, body
		FROM review_requests WHERE invocation_id = ?`, id).Scan(&runID, &round, &base, &head, &requested, &digest, &body)
	if err != nil {
		return domain.ReviewRequestRecord{}, notFoundOr(err)
	}
	var r domain.ReviewRequestRecord
	if err := json.Unmarshal(body, &r); err != nil {
		return r, err
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	if digest != reviewBodyDigest(string(body)) || r.InvocationID != id || string(r.RunID) != runID ||
		r.Round != round || r.BaseSHA != base || r.HeadSHA != head || formatTime(r.RequestedAt) != requested {
		return r, fmt.Errorf("review request %q: %w", id, errRowInconsistent)
	}
	if err := tx.ensureInvocationNotShadowRecorded(ctx, id); err != nil {
		return r, err
	}
	return r, nil
}

// ListReviewRequests authenticates copied keys before filtering, as the
// terminal review readers do, so a corrupt run key cannot hide history.
func (tx *ReadTx) ListReviewRequests(ctx context.Context, runID domain.RunID) ([]domain.ReviewRequestRecord, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT invocation_id FROM review_requests ORDER BY run_id, round`)
	if err != nil {
		return nil, err
	}
	var ids []domain.InvocationID
	for rows.Next() {
		var id domain.InvocationID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]domain.ReviewRequestRecord, 0)
	for _, id := range ids {
		r, err := tx.GetReviewRequest(ctx, id)
		if err != nil {
			return nil, err
		}
		if r.RunID == runID {
			result = append(result, r)
		}
	}
	return result, nil
}
