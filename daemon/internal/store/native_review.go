package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	insertNativeReviewObservationSQL = `INSERT INTO native_review_observations
		(repository_id, pr_number, provider, kind, native_id, body, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`
	latestNativeReviewObservationSQL = `SELECT repository_id, pr_number, provider, kind, native_id, body, observed_at
		FROM native_review_observations
		WHERE repository_id = ? AND pr_number = ? AND provider = ? AND kind = ? AND native_id = ?
		ORDER BY id DESC LIMIT 1`
	listNativeReviewObservationsSQL = `SELECT repository_id, pr_number, provider, kind, native_id, body, observed_at
		FROM native_review_observations
		WHERE repository_id = ? AND pr_number = ?
		ORDER BY id`
)

// AppendNativeReviewObservation appends one native review observation under the
// same material-change rule as AppendPullMergeFact: a re-poll of unchanged
// native state coalesces (returns false, no row), a material change appends.
// The identity is (repository_id, pr_number, provider, kind, native_id), so
// each native review or reaction has its own convergent timeline and duplicate
// observations no-op under retries.
func (tx *InternalTx) AppendNativeReviewObservation(ctx context.Context, o domain.NativeReviewObservation) (bool, error) {
	body, err := encode(o)
	if err != nil {
		return false, fmt.Errorf("append native review observation: %w", err)
	}
	latest, err := tx.scanNativeReviewObservation(tx.tx.QueryRowContext(ctx, latestNativeReviewObservationSQL,
		o.RepositoryID, o.PRNumber, string(o.Provider), string(o.Kind), o.NativeID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("append native review observation %s#%d %s/%d: %w",
			o.Repo, o.PRNumber, o.Kind, o.NativeID, err)
	default:
		if !o.MaterialChangeFrom(latest) {
			return false, nil
		}
	}
	if _, err := tx.tx.ExecContext(ctx, insertNativeReviewObservationSQL,
		o.RepositoryID, o.PRNumber, string(o.Provider), string(o.Kind), o.NativeID,
		body, formatTime(o.ObservedAt)); err != nil {
		return false, fmt.Errorf("append native review observation %s#%d %s/%d: %w",
			o.Repo, o.PRNumber, o.Kind, o.NativeID, err)
	}
	return true, nil
}

// scanNativeReviewObservation reconstructs one native review observation row,
// cross-checking the decoded body against the stamped key columns. decode
// re-runs Validate (the trust re-gate over third-party review content), so a
// tampered or malformed row fails closed here.
func (tx *ReadTx) scanNativeReviewObservation(sc scanner) (domain.NativeReviewObservation, error) {
	var (
		repositoryID, nativeID int64
		prNumber               int64
		provider, kind         string
		observedAt             string
		body                   []byte
	)
	if err := sc.Scan(&repositoryID, &prNumber, &provider, &kind, &nativeID, &body, &observedAt); err != nil {
		return domain.NativeReviewObservation{}, err
	}
	o, err := decode[domain.NativeReviewObservation](body)
	if err != nil {
		return domain.NativeReviewObservation{}, err
	}
	if o.RepositoryID != repositoryID || int64(o.PRNumber) != prNumber ||
		string(o.Provider) != provider || string(o.Kind) != kind || o.NativeID != nativeID ||
		formatTime(o.ObservedAt) != observedAt {
		return domain.NativeReviewObservation{}, errRowInconsistent
	}
	return o, nil
}

// LatestNativeReviewObservation returns the newest recorded observation for one
// native review identity, or ErrNotFound before any observation.
func (tx *ReadTx) LatestNativeReviewObservation(
	ctx context.Context, repositoryID int64, prNumber int,
	provider domain.NativeReviewProvider, kind domain.NativeReviewKind, nativeID int64,
) (domain.NativeReviewObservation, error) {
	o, err := tx.scanNativeReviewObservation(tx.tx.QueryRowContext(ctx, latestNativeReviewObservationSQL,
		repositoryID, prNumber, string(provider), string(kind), nativeID))
	if err != nil {
		return domain.NativeReviewObservation{}, fmt.Errorf("latest native review observation %d#%d %s/%d: %w",
			repositoryID, prNumber, kind, nativeID, notFoundOr(err))
	}
	return o, nil
}

// ListNativeReviewObservations returns one pull request's native review
// timeline across every provider, kind, and identity in append order.
func (tx *ReadTx) ListNativeReviewObservations(
	ctx context.Context, repositoryID int64, prNumber int,
) ([]domain.NativeReviewObservation, error) {
	rows, err := tx.tx.QueryContext(ctx, listNativeReviewObservationsSQL, repositoryID, prNumber)
	if err != nil {
		return nil, fmt.Errorf("list native review observations %d#%d: %w", repositoryID, prNumber, err)
	}
	defer func() { _ = rows.Close() }()
	var observations []domain.NativeReviewObservation
	for rows.Next() {
		o, err := tx.scanNativeReviewObservation(rows)
		if err != nil {
			return nil, fmt.Errorf("list native review observations %d#%d: %w", repositoryID, prNumber, err)
		}
		observations = append(observations, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list native review observations %d#%d: %w", repositoryID, prNumber, err)
	}
	return observations, nil
}
