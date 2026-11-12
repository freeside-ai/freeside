package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrEnrollmentNotOnPool is returned when a usage record names an enrollment
// whose identity meters a different pool. The provider's answer under that
// credential describes another allowance.
var ErrEnrollmentNotOnPool = errors.New("enrollment is not on the usage pool")

// ErrReadingGenerationReplaced is returned when a new window reading names a
// generation other than the collector's current one. A later generation has
// replaced what it was taken under, so collection discards it.
var ErrReadingGenerationReplaced = errors.New(
	"window reading is not stamped with the collector's current enrollment generation")

const (
	recordPoolUsageObservationSQL = `
INSERT INTO pool_usage_observations
    (pool, collector, last_attempt_status, last_attempt_at, recorded_at, body, as_of_revision, entity_version)
VALUES (?, ?, ?, ?, ?, ?, ?, 1)
ON CONFLICT (pool) DO UPDATE SET
    collector           = excluded.collector,
    last_attempt_status = excluded.last_attempt_status,
    last_attempt_at     = excluded.last_attempt_at,
    recorded_at         = excluded.recorded_at,
    body                = excluded.body,
    as_of_revision      = excluded.as_of_revision,
    entity_version      = pool_usage_observations.entity_version + 1`
	getPoolUsageObservationSQL = `
SELECT collector, last_attempt_status, last_attempt_at, recorded_at, body, as_of_revision, entity_version
FROM pool_usage_observations WHERE pool = ?`
	deletePoolUsageObservationSQL    = `DELETE FROM pool_usage_observations WHERE pool = ?`
	listPoolUsageObservationPoolsSQL = `SELECT pool FROM pool_usage_observations ORDER BY pool`
	clientEnrollmentRecordedAtSQL    = `SELECT recorded_at FROM client_enrollments WHERE id = ?`
)

// poolUsageObservationRecord is the stored body: the record plus the instant
// that orders its revisions.
type poolUsageObservationRecord struct {
	Observation domain.PoolUsageObservation `json:"observation"`
	RecordedAt  time.Time                   `json:"recorded_at"`
}

// Validate reports whether a stored body is a complete revision.
func (r poolUsageObservationRecord) Validate() error {
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("pool usage observation %q recorded_at: %w", r.Observation.Pool, domain.ErrMissingTimestamp)
	}
	return r.Observation.Validate()
}

// RecordPoolUsageObservation persists a pool's whole record, replacing any
// earlier revision: a caller that means to keep what the record holds reads,
// changes, and writes it in one transaction. Every named enrollment must be on
// the pool, and a refusal mark an allowed reading has cleared is dropped. A
// reading the stored record already holds is carried: it keeps naming its
// generation, so a failed refresh or a partial update can rewrite the record
// after a later generation. Any other reading is new. It needs a succeeded
// attempt and the collector's current generation, and must be observed after
// the one it replaces.
//
// Clients sync the record (plan §5.14), so it is written only in a
// revision-bumping transaction.
func (tx *WriteTx) RecordPoolUsageObservation(
	ctx context.Context, observation domain.PoolUsageObservation, recordedAt time.Time,
) error {
	fail := func(err error) error {
		return fmt.Errorf("record pool usage observation %q: %w", observation.Pool, err)
	}
	observation = observation.WithoutClearedMarks()
	record := poolUsageObservationRecord{Observation: observation, RecordedAt: recordedAt.UTC()}
	if err := record.Validate(); err != nil {
		return fail(err)
	}
	var carried []domain.WindowReading
	stored, _, err := tx.getPoolUsageObservationRecord(ctx, observation.Pool)
	switch {
	case err == nil:
		// As for a pool revision: a delayed older record must not reinstate a
		// superseded one, and a shared instant carries no ordering evidence.
		if recordedAt.Before(stored.RecordedAt) ||
			(recordedAt.Equal(stored.RecordedAt) && !reflect.DeepEqual(stored.Observation, observation)) {
			return fail(fmt.Errorf("revision stamped %s, stored revision is %s: %w",
				formatTime(recordedAt), formatTime(stored.RecordedAt), ErrStaleWrite))
		}
		carried = stored.Observation.Windows
	case !errors.Is(err, ErrNotFound):
		return fail(err)
	}
	if err := tx.requireNewReadingsCurrent(ctx, observation, carried); err != nil {
		return fail(err)
	}
	return tx.writePoolUsageObservation(ctx, record)
}

// writePoolUsageObservation upserts a record that passed its caller's ordering
// gate, holding every enrollment it names to the pool.
func (tx *WriteTx) writePoolUsageObservation(ctx context.Context, record poolUsageObservationRecord) error {
	observation := record.Observation
	fail := func(err error) error {
		return fmt.Errorf("record pool usage observation %q: %w", observation.Pool, err)
	}
	body, err := encode(record)
	if err != nil {
		return fail(err)
	}
	if err := tx.requireEnrollmentsOnPool(ctx, observation); err != nil {
		return fail(err)
	}
	var attemptStatus, attemptAt any
	if attempt := observation.LastAttempt; attempt != nil {
		attemptStatus, attemptAt = string(attempt.Status), formatTime(attempt.At)
	}
	if _, err := tx.tx.ExecContext(ctx, recordPoolUsageObservationSQL, observation.Pool, observation.Collector,
		attemptStatus, attemptAt, formatTime(record.RecordedAt), body, tx.asOfRevision); err != nil {
		return fail(err)
	}
	return nil
}

// requireNewReadingsCurrent checks each reading the stored record doesn't
// already hold, which domain validation can't tell from a kept one: a failed
// attempt can't produce a reading, a replaced generation's reading is
// discarded, and a reading observed earlier must not undo a later one.
func (tx *ReadTx) requireNewReadingsCurrent(
	ctx context.Context, observation domain.PoolUsageObservation, carried []domain.WindowReading,
) error {
	for _, reading := range observation.Windows {
		if slices.ContainsFunc(carried, func(stored domain.WindowReading) bool {
			return reflect.DeepEqual(stored, reading)
		}) {
			continue
		}
		if observation.LastAttempt.Status != domain.CollectionAttemptSucceeded {
			return fmt.Errorf("new reading for window %s under a %s attempt: %w",
				reading.Window, observation.LastAttempt.Status, domain.ErrSyntheticWindow)
		}
		current, err := tx.CurrentEnrollmentGeneration(ctx, observation.Collector)
		if err != nil {
			return err
		}
		if reading.Generation != current.Ordinal {
			return fmt.Errorf("window %s stamped generation %d, collector %s is at %d: %w",
				reading.Window, reading.Generation, observation.Collector, current.Ordinal,
				ErrReadingGenerationReplaced)
		}
		for _, held := range carried {
			if held.Window == reading.Window && !reading.ObservedAt.After(held.ObservedAt) {
				return fmt.Errorf("window %s observed %s, stored reading is %s: %w", reading.Window,
					formatTime(reading.ObservedAt), formatTime(held.ObservedAt), ErrStaleWrite)
			}
		}
	}
	return nil
}

// RecordWindowRefusalMark records that a provider refusal marked a window
// exhausted. A pool with no record yet gets one, with its default collector
// and no readings. The read and the write share one transaction, so the
// revision ordering of RecordPoolUsageObservation has nothing to protect here:
// a late or same-instant caller must not lose a provider refusal, and the
// record keeps the later instant. The mark's own order decides which mark a
// window holds (PoolUsageObservation.WithRefusalMark). Like any record write,
// it raises the revision.
func (tx *WriteTx) RecordWindowRefusalMark(
	ctx context.Context, pool domain.UsagePoolID, mark domain.RefusalMark, recordedAt time.Time,
) error {
	fail := func(err error) error {
		return fmt.Errorf("record window refusal mark on %q: %w", pool, err)
	}
	if recordedAt.IsZero() {
		return fail(domain.ErrMissingTimestamp)
	}
	record := poolUsageObservationRecord{RecordedAt: recordedAt.UTC()}
	stored, _, err := tx.getPoolUsageObservationRecord(ctx, pool)
	switch {
	case err == nil:
		record.Observation = stored.Observation
		if stored.RecordedAt.After(record.RecordedAt) {
			record.RecordedAt = stored.RecordedAt
		}
	case errors.Is(err, ErrNotFound):
		collector, err := tx.DefaultPoolCollector(ctx, pool)
		if err != nil {
			return fail(err)
		}
		record.Observation = domain.PoolUsageObservation{Pool: pool, Collector: collector}
	default:
		return fail(err)
	}
	record.Observation = record.Observation.WithRefusalMark(mark)
	return tx.writePoolUsageObservation(ctx, record)
}

// deletePoolUsageObservation removes a pool's record. A derived pool loses its
// one identity when that identity is characterized into a shared pool: no
// enrollment can meet the record's gate again, and a record that fails it
// would fail every listing. Readings return with the next collection and a
// refusal with the next provider refusal, on the pool that identity now meters.
func (tx *WriteTx) deletePoolUsageObservation(ctx context.Context, pool domain.UsagePoolID) error {
	if _, err := tx.tx.ExecContext(ctx, deletePoolUsageObservationSQL, pool); err != nil {
		return fmt.Errorf("delete pool usage observation %q: %w", pool, err)
	}
	return nil
}

// DefaultPoolCollector returns the pool's first enrollment: the earliest
// recorded, ties by id. ErrNotFound means the pool has no enrollment.
func (tx *ReadTx) DefaultPoolCollector(
	ctx context.Context, pool domain.UsagePoolID,
) (domain.ClientEnrollmentID, error) {
	fail := func(err error) (domain.ClientEnrollmentID, error) {
		return "", fmt.Errorf("default collector of %q: %w", pool, err)
	}
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return fail(err)
	}
	var (
		first   domain.ClientEnrollmentID
		firstAt time.Time
	)
	for _, identity := range identities {
		if identity.EffectiveUsagePool() != pool {
			continue
		}
		enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
		if err != nil {
			return fail(err)
		}
		for _, enrollment := range enrollments {
			var column string
			if err := tx.tx.QueryRowContext(ctx, clientEnrollmentRecordedAtSQL, enrollment.ID).Scan(&column); err != nil {
				return fail(err)
			}
			recordedAt, err := parseTime(column)
			if err != nil {
				return fail(err)
			}
			if first == "" || recordedAt.Before(firstAt) ||
				(recordedAt.Equal(firstAt) && enrollment.ID < first) {
				first, firstAt = enrollment.ID, recordedAt
			}
		}
	}
	if first == "" {
		return fail(ErrNotFound)
	}
	return first, nil
}

// GetPoolUsageObservation reconstructs one pool's record, cross-checking the
// extracted columns against the decoded body and re-running the provenance
// gates: every enrollment is on the pool, and every reading names a generation
// the collector really has. A reading may predate a later generation, so the
// read doesn't require the current one. A body naming what doesn't exist is an
// inconsistent row, never ErrNotFound: that would read as a pool with no record.
func (tx *ReadTx) GetPoolUsageObservation(
	ctx context.Context, pool domain.UsagePoolID,
) (domain.PoolUsageObservation, error) {
	snapshotted, err := tx.GetPoolUsageObservationSnapshot(ctx, pool)
	return snapshotted.Value, err
}

// GetPoolUsageObservationSnapshot is GetPoolUsageObservation with the row's
// sync metadata: the revision that last wrote the record and its version.
func (tx *ReadTx) GetPoolUsageObservationSnapshot(
	ctx context.Context, pool domain.UsagePoolID,
) (Snapshotted[domain.PoolUsageObservation], error) {
	record, snapshot, err := tx.getPoolUsageObservationRecord(ctx, pool)
	if err != nil {
		return Snapshotted[domain.PoolUsageObservation]{}, err
	}
	observation := record.Observation
	err = tx.requireEnrollmentsOnPool(ctx, observation)
	for i := 0; err == nil && i < len(observation.Windows); i++ {
		_, err = tx.GetEnrollmentGeneration(ctx, observation.Collector, observation.Windows[i].Generation)
	}
	if errors.Is(err, ErrNotFound) {
		err = errRowInconsistent
	}
	if err != nil {
		return Snapshotted[domain.PoolUsageObservation]{}, fmt.Errorf("get pool usage observation %q: %w", pool, err)
	}
	return Snapshotted[domain.PoolUsageObservation]{Value: observation, Snapshot: snapshot}, nil
}

// getPoolUsageObservationRecord reads one row without the enrollment gate: a
// write gates the record it writes, not the one it replaces.
func (tx *ReadTx) getPoolUsageObservationRecord(
	ctx context.Context, pool domain.UsagePoolID,
) (poolUsageObservationRecord, Snapshot, error) {
	var (
		collector  string
		status     sql.NullString
		attemptAt  sql.NullString
		recordedAt string
		body       []byte
		snapshot   Snapshot
	)
	err := tx.tx.QueryRowContext(ctx, getPoolUsageObservationSQL, pool).
		Scan(&collector, &status, &attemptAt, &recordedAt, &body, &snapshot.AsOfRevision, &snapshot.EntityVersion)
	if err != nil {
		return poolUsageObservationRecord{}, Snapshot{}, fmt.Errorf("get pool usage observation %q: %w", pool, notFoundOr(err))
	}
	record, err := decode[poolUsageObservationRecord](body)
	if err != nil {
		return poolUsageObservationRecord{}, Snapshot{}, fmt.Errorf("get pool usage observation %q: %w", pool, err)
	}
	observation := record.Observation
	attemptMatches := !status.Valid && !attemptAt.Valid && observation.LastAttempt == nil
	if attempt := observation.LastAttempt; attempt != nil {
		attemptMatches = status.Valid && status.String == string(attempt.Status) &&
			optionalTimeColumnEqual(attemptAt, &attempt.At)
	}
	// Every write stamps both from 1, so a zero is a row no writer produced.
	if observation.Pool != pool || string(observation.Collector) != collector || !attemptMatches ||
		!timeColumnEqual(recordedAt, record.RecordedAt) || snapshot.AsOfRevision < 1 || snapshot.EntityVersion < 1 {
		return poolUsageObservationRecord{}, Snapshot{}, fmt.Errorf("get pool usage observation %q: %w", pool, errRowInconsistent)
	}
	return record, snapshot, nil
}

// ListPoolUsageObservations reconstructs every record in pool id order; one
// that fails its gates fails the whole listing.
func (tx *ReadTx) ListPoolUsageObservations(ctx context.Context) ([]domain.PoolUsageObservation, error) {
	snapshots, err := tx.ListPoolUsageObservationSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	observations := make([]domain.PoolUsageObservation, 0, len(snapshots))
	for _, snapshotted := range snapshots {
		observations = append(observations, snapshotted.Value)
	}
	return observations, nil
}

// ListPoolUsageObservationSnapshots is ListPoolUsageObservations with each
// row's sync metadata.
func (tx *ReadTx) ListPoolUsageObservationSnapshots(
	ctx context.Context,
) ([]Snapshotted[domain.PoolUsageObservation], error) {
	pools, err := queryIDs[domain.UsagePoolID](ctx, tx, listPoolUsageObservationPoolsSQL)
	if err != nil {
		return nil, fmt.Errorf("list pool usage observations: %w", err)
	}
	snapshots := make([]Snapshotted[domain.PoolUsageObservation], 0, len(pools))
	for _, pool := range pools {
		snapshotted, err := tx.GetPoolUsageObservationSnapshot(ctx, pool)
		if err != nil {
			return nil, fmt.Errorf("list pool usage observations: %w", err)
		}
		snapshots = append(snapshots, snapshotted)
	}
	return snapshots, nil
}

// RequireEnrollmentOnPool reports whether an enrollment's identity meters the
// pool: ErrNotFound when the enrollment doesn't exist, ErrEnrollmentNotOnPool
// when its identity meters another. A collector must pass it before a
// reassignment names it.
func (tx *ReadTx) RequireEnrollmentOnPool(
	ctx context.Context, pool domain.UsagePoolID, id domain.ClientEnrollmentID,
) error {
	return tx.requireEnrollmentsOnPool(ctx, domain.PoolUsageObservation{Pool: pool, Collector: id})
}

// requireEnrollmentsOnPool holds the collector and every availability answer
// to the identities' current pool.
func (tx *ReadTx) requireEnrollmentsOnPool(ctx context.Context, observation domain.PoolUsageObservation) error {
	named := []domain.ClientEnrollmentID{observation.Collector}
	for _, availability := range observation.Enrollments {
		named = append(named, availability.EnrollmentID)
	}
	for _, id := range named {
		enrollment, err := tx.GetClientEnrollment(ctx, id)
		if err != nil {
			return err
		}
		identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
		if err != nil {
			return err
		}
		if identity.EffectiveUsagePool() != observation.Pool {
			return fmt.Errorf("enrollment %s meters pool %s, record is pool %s: %w",
				id, identity.EffectiveUsagePool(), observation.Pool, ErrEnrollmentNotOnPool)
		}
	}
	return nil
}

// decodePoolUsageObservationRows reads every pool usage observation for the
// stored-row check (stored_rows.go) and discards them. It lives here because
// the pool usage guard lets only allowlisted files name the record's readers.
func decodePoolUsageObservationRows(ctx context.Context, s *Store) error {
	return s.Read(ctx, func(tx *ReadTx) error {
		return discard(tx.ListPoolUsageObservationSnapshots(ctx))
	})
}
