package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordImageUpkeepRefusalSQL = `
INSERT INTO image_upkeep_refusals (id, repository_id, harness, tip_sha, class, recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING`
	getImageUpkeepRefusalSQL = `
SELECT id, repository_id, harness, tip_sha, class, recorded_at, body
FROM image_upkeep_refusals WHERE id = ?`
	listImageUpkeepRefusalsSQL = `
SELECT id, repository_id, harness, tip_sha, class, recorded_at, body
FROM image_upkeep_refusals WHERE repository_id = ? ORDER BY rowid`
	recordRepositoryTipObservationSQL = `
INSERT INTO repository_tip_observations
    (repository_id, branch, tip_sha, observed_at, upkeep_tip_sha, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (repository_id) DO UPDATE SET
    branch = excluded.branch,
    tip_sha = excluded.tip_sha,
    observed_at = excluded.observed_at,
    upkeep_tip_sha = excluded.upkeep_tip_sha,
    body = excluded.body`
	getRepositoryTipObservationSQL = `
SELECT repository_id, branch, tip_sha, observed_at, upkeep_tip_sha, body
FROM repository_tip_observations WHERE repository_id = ?`
)

// RecordImageUpkeepRefusal writes one immutable upkeep refusal and returns
// the stored record. The ID addresses every fact but RecordedAt, so a rerun
// that refuses the same way at the same tip converges on the first row and
// gets back its original RecordedAt; putImmutable's byte-identical replay
// rule would instead turn every rerun into a conflict. A different refusal
// cannot share an ID, so an existing row that decodes to other facts is a
// corrupt row and fails closed. Like project images, the record is
// daemon-internal, so this belongs on InternalTx and does not bump the
// server revision.
func (tx *InternalTx) RecordImageUpkeepRefusal(
	ctx context.Context, refusal domain.ImageUpkeepRefusal,
) (domain.ImageUpkeepRefusal, error) {
	body, err := encode(refusal)
	if err != nil {
		return domain.ImageUpkeepRefusal{}, fmt.Errorf("record image upkeep refusal %q: %w", refusal.ID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordImageUpkeepRefusalSQL,
		refusal.ID, refusal.RepositoryID, refusal.Harness, refusal.TipSHA, refusal.Class,
		formatTime(refusal.RecordedAt), body,
	); err != nil {
		return domain.ImageUpkeepRefusal{}, fmt.Errorf("record image upkeep refusal %q: %w", refusal.ID, err)
	}
	stored, err := tx.GetImageUpkeepRefusal(ctx, refusal.ID)
	if err != nil {
		return domain.ImageUpkeepRefusal{}, fmt.Errorf("record image upkeep refusal: %w", err)
	}
	return stored, nil
}

// GetImageUpkeepRefusal reconstructs one upkeep refusal record. Decoding
// recomputes the content address, and the indexed columns must agree with
// the body, so neither can be altered alone.
func (tx *ReadTx) GetImageUpkeepRefusal(
	ctx context.Context, id domain.Digest,
) (domain.ImageUpkeepRefusal, error) {
	refusal, err := scanImageUpkeepRefusal(tx.tx.QueryRowContext(ctx, getImageUpkeepRefusalSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ImageUpkeepRefusal{}, fmt.Errorf("get image upkeep refusal %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return domain.ImageUpkeepRefusal{}, fmt.Errorf("get image upkeep refusal %q: %w", id, err)
	}
	if refusal.ID != id {
		return domain.ImageUpkeepRefusal{}, fmt.Errorf("get image upkeep refusal %q: %w", id, errRowInconsistent)
	}
	return refusal, nil
}

// ListImageUpkeepRefusals returns one repository's upkeep refusals in the
// order they were first recorded.
func (tx *ReadTx) ListImageUpkeepRefusals(
	ctx context.Context, repositoryID int64,
) ([]domain.ImageUpkeepRefusal, error) {
	rows, err := tx.tx.QueryContext(ctx, listImageUpkeepRefusalsSQL, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("list image upkeep refusals for repository %d: %w", repositoryID, err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err reports deferred-close failures
	var refusals []domain.ImageUpkeepRefusal
	for rows.Next() {
		refusal, err := scanImageUpkeepRefusal(rows)
		if err != nil {
			return nil, fmt.Errorf("list image upkeep refusals for repository %d: %w", repositoryID, err)
		}
		if refusal.RepositoryID != repositoryID {
			return nil, fmt.Errorf("list image upkeep refusals for repository %d: %w",
				repositoryID, errRowInconsistent)
		}
		refusals = append(refusals, refusal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list image upkeep refusals for repository %d: %w", repositoryID, err)
	}
	return refusals, nil
}

func scanImageUpkeepRefusal(row scanner) (domain.ImageUpkeepRefusal, error) {
	var (
		id, harness, tipSHA, class, recordedAt string
		repositoryID                           int64
		body                                   []byte
	)
	if err := row.Scan(&id, &repositoryID, &harness, &tipSHA, &class, &recordedAt, &body); err != nil {
		return domain.ImageUpkeepRefusal{}, err
	}
	refusal, err := decode[domain.ImageUpkeepRefusal](body)
	if err != nil {
		return domain.ImageUpkeepRefusal{}, err
	}
	if string(refusal.ID) != id || refusal.RepositoryID != repositoryID ||
		string(refusal.Harness) != harness || refusal.TipSHA != tipSHA ||
		string(refusal.Class) != class || formatTime(refusal.RecordedAt) != recordedAt {
		return domain.ImageUpkeepRefusal{}, errRowInconsistent
	}
	return refusal, nil
}

// RecordRepositoryTipObservation records the latest default-branch tip read
// for one repository, replacing the previous observation. It is
// daemon-internal and does not bump the server revision, so it is a Record,
// not a Put: Put names a synchronized write.
func (tx *InternalTx) RecordRepositoryTipObservation(
	ctx context.Context, observation domain.RepositoryTipObservation,
) error {
	body, err := encode(observation)
	if err != nil {
		return fmt.Errorf("record repository tip observation %d: %w", observation.RepositoryID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordRepositoryTipObservationSQL,
		observation.RepositoryID, observation.Branch, observation.TipSHA,
		formatTime(observation.ObservedAt), nullableString(observation.UpkeepTipSHA), body,
	); err != nil {
		return fmt.Errorf("record repository tip observation %d: %w", observation.RepositoryID, err)
	}
	return nil
}

// GetRepositoryTipObservation reconstructs one repository's latest tip
// observation, reporting absence rather than erroring.
func (tx *ReadTx) GetRepositoryTipObservation(
	ctx context.Context, repositoryID int64,
) (domain.RepositoryTipObservation, bool, error) {
	var (
		rowRepositoryID            int64
		branch, tipSHA, observedAt string
		upkeepTipSHA               sql.NullString
		body                       []byte
	)
	err := tx.tx.QueryRowContext(ctx, getRepositoryTipObservationSQL, repositoryID).
		Scan(&rowRepositoryID, &branch, &tipSHA, &observedAt, &upkeepTipSHA, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepositoryTipObservation{}, false, nil
	}
	if err != nil {
		return domain.RepositoryTipObservation{}, false,
			fmt.Errorf("get repository tip observation %d: %w", repositoryID, err)
	}
	observation, err := decode[domain.RepositoryTipObservation](body)
	if err != nil {
		return domain.RepositoryTipObservation{}, false,
			fmt.Errorf("get repository tip observation %d: %w", repositoryID, err)
	}
	if observation.RepositoryID != repositoryID || rowRepositoryID != repositoryID ||
		observation.Branch != branch || observation.TipSHA != tipSHA ||
		formatTime(observation.ObservedAt) != observedAt ||
		!optionalStringEqual(upkeepTipSHA, observation.UpkeepTipSHA) {
		return domain.RepositoryTipObservation{}, false,
			fmt.Errorf("get repository tip observation %d: %w", repositoryID, errRowInconsistent)
	}
	return observation, true, nil
}
