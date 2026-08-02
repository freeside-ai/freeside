package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Durable scheduler persistence (plan §5.16, issue #442), split by
// visibility. The schedules aggregate is synchronized state: Put lives on
// WriteTx and every update rides a revision bump. The timer clock and the
// occurrence ledger are daemon-internal bookkeeping on InternalTx (the 0014
// rule), so a recurring fire does not invalidate client caches; migration
// 0025 records the split's rationale.

const putScheduleSQL = `
INSERT INTO schedules
    (id, project_id, kind, status, generation, run_id, policy_digest,
     fire_at, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    project_id     = excluded.project_id,
    kind           = excluded.kind,
    status         = excluded.status,
    generation     = excluded.generation,
    run_id         = excluded.run_id,
    policy_digest  = excluded.policy_digest,
    fire_at        = excluded.fire_at,
    entity_version = schedules.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

// PutSchedule upserts one schedule aggregate, enforcing the domain's
// transition rules against the stored row (identity fixed, terminal
// generations frozen, re-arms advancing the generation by exactly one).
func (tx *WriteTx) PutSchedule(ctx context.Context, schedule domain.Schedule) error {
	body, err := encode(schedule)
	if err != nil {
		return fmt.Errorf("put schedule %q: %w", schedule.ID, err)
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM schedules WHERE id = ?`, schedule.ID)
	if err != nil {
		return fmt.Errorf("put schedule %q: %w", schedule.ID, err)
	}
	if existing != nil {
		old, err := decode[domain.Schedule](existing)
		if err != nil {
			return fmt.Errorf("put schedule %q: %w", schedule.ID, err)
		}
		if err := domain.ValidateScheduleTransition(old, schedule); err != nil {
			return fmt.Errorf("put schedule %q: %w", schedule.ID, mapTransition(err))
		}
	}
	var runID, policyDigest, fireAt any
	if schedule.RunID != nil {
		runID = *schedule.RunID
	}
	if schedule.PolicyDigest != nil {
		policyDigest = *schedule.PolicyDigest
	}
	if schedule.FireAt != nil {
		fireAt = schedule.FireAt.UnixNano()
	}
	if _, err := tx.tx.ExecContext(ctx, putScheduleSQL,
		schedule.ID, schedule.ProjectID, schedule.Kind, schedule.Status,
		schedule.Generation, runID, policyDigest,
		fireAt, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put schedule %q: %w", schedule.ID, err)
	}
	return nil
}

// migrateLegacyRunPolicySchedules moves every schedule bound to the exact
// legacy run digest with the run's authenticated policy migration. The
// schedule contract keeps this binding immutable during ordinary operation;
// this narrowly gated upgrade path is the only exception.
func (tx *WriteTx) migrateLegacyRunPolicySchedules(
	ctx context.Context,
	legacy, updated domain.Run,
) error {
	rows, err := tx.tx.QueryContext(ctx, `
SELECT id, project_id, kind, status, generation, run_id, policy_digest,
       fire_at, entity_version, as_of_revision, body
FROM schedules WHERE run_id = ? ORDER BY id`, legacy.ID)
	if err != nil {
		return fmt.Errorf("list schedules for legacy run %q: %w", legacy.ID, err)
	}
	defer func() { _ = rows.Close() }()

	var schedules []domain.Schedule
	for rows.Next() {
		schedule, _, err := tx.scanScheduleSnapshot(rows)
		if err != nil {
			return fmt.Errorf("scan schedule for legacy run %q: %w", legacy.ID, err)
		}
		if schedule.ProjectID != legacy.ProjectID || schedule.RunID == nil ||
			*schedule.RunID != legacy.ID || schedule.PolicyDigest == nil ||
			*schedule.PolicyDigest != legacy.PolicyDigest {
			return fmt.Errorf(
				"schedule %q disagrees with legacy run policy: %w",
				schedule.ID, domain.ErrImmutableTransition,
			)
		}
		schedules = append(schedules, schedule)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list schedules for legacy run %q: %w", legacy.ID, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close schedules for legacy run %q: %w", legacy.ID, err)
	}

	for _, schedule := range schedules {
		policyDigest := updated.PolicyDigest
		schedule.PolicyDigest = &policyDigest
		body, err := encode(schedule)
		if err != nil {
			return fmt.Errorf("migrate schedule %q run policy: %w", schedule.ID, err)
		}
		result, err := tx.tx.ExecContext(ctx, `
UPDATE schedules
SET policy_digest = ?, entity_version = entity_version + 1,
    as_of_revision = ?, body = ?
WHERE id = ? AND run_id = ? AND policy_digest = ?`,
			updated.PolicyDigest, tx.asOfRevision, body,
			schedule.ID, legacy.ID, legacy.PolicyDigest,
		)
		if err != nil {
			return fmt.Errorf("migrate schedule %q run policy: %w", schedule.ID, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("migrate schedule %q rows affected: %w", schedule.ID, err)
		}
		if changed != 1 {
			return fmt.Errorf("migrate schedule %q changed %d rows: %w",
				schedule.ID, changed, domain.ErrImmutableTransition)
		}
	}
	return nil
}

// scanScheduleSnapshot reconstructs one schedules row (see the scanner doc
// for the shared gate sequence). Errors are returned unwrapped; callers add
// the entity/key context.
func (tx *ReadTx) scanScheduleSnapshot(sc scanner) (domain.Schedule, Snapshot, error) {
	var (
		id         string
		projectID  string
		kind       string
		status     string
		generation int64
		runID      sql.NullString
		policy     sql.NullString
		fireAt     sql.NullInt64
		snap       Snapshot
		body       []byte
	)
	if err := sc.Scan(&id, &projectID, &kind, &status, &generation, &runID, &policy, &fireAt,
		&snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
		return domain.Schedule{}, Snapshot{}, err
	}
	schedule, err := decode[domain.Schedule](body)
	if err != nil {
		return domain.Schedule{}, Snapshot{}, err
	}
	storedRunID := schedule.RunID != nil
	storedPolicy := schedule.PolicyDigest != nil
	storedFireAt := schedule.FireAt != nil
	if schedule.ID != domain.ScheduleID(id) || schedule.ProjectID != domain.ProjectID(projectID) ||
		schedule.Kind != domain.ScheduleKind(kind) || schedule.Status != domain.ScheduleStatus(status) ||
		schedule.Generation != generation || storedRunID != runID.Valid || storedPolicy != policy.Valid ||
		(storedRunID && *schedule.RunID != domain.RunID(runID.String)) ||
		(storedPolicy && *schedule.PolicyDigest != domain.Digest(policy.String)) ||
		storedFireAt != fireAt.Valid ||
		(storedFireAt && schedule.FireAt.UnixNano() != fireAt.Int64) ||
		snap.EntityVersion < 1 || snap.AsOfRevision < 1 {
		return domain.Schedule{}, Snapshot{}, errRowInconsistent
	}
	return schedule, snap, nil
}

const getScheduleSQL = `
SELECT id, project_id, kind, status, generation, run_id, policy_digest,
       fire_at, entity_version, as_of_revision, body
FROM schedules WHERE id = ?`

// GetSchedule returns one schedule aggregate.
func (tx *ReadTx) GetSchedule(ctx context.Context, id domain.ScheduleID) (domain.Schedule, error) {
	schedule, _, err := tx.GetScheduleSnapshot(ctx, id)
	return schedule, err
}

// GetScheduleSnapshot additionally returns the store-stamped sync metadata.
func (tx *ReadTx) GetScheduleSnapshot(ctx context.Context, id domain.ScheduleID) (domain.Schedule, Snapshot, error) {
	schedule, snap, err := tx.scanScheduleSnapshot(tx.tx.QueryRowContext(ctx, getScheduleSQL, id))
	if err != nil {
		return domain.Schedule{}, Snapshot{}, fmt.Errorf("get schedule %q: %w", id, notFoundOr(err))
	}
	return schedule, snap, nil
}

const listSchedulesSQL = `
SELECT id, project_id, kind, status, generation, run_id, policy_digest,
       fire_at, entity_version, as_of_revision, body
FROM schedules ORDER BY id`

// ListSchedules enumerates every persisted schedule (List semantics in
// list.go).
func (tx *ReadTx) ListSchedules(ctx context.Context) ([]Snapshotted[domain.Schedule], error) {
	schedules, err := listSnapshotted(ctx, tx, listSchedulesSQL, (*ReadTx).scanScheduleSnapshot)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return schedules, nil
}

// DueSchedule pairs an armed schedule with the nominal instant that made it
// due: the one-shot deadline itself, or the recurring timer's next nominal
// fire.
type DueSchedule struct {
	Schedule      domain.Schedule
	NominalFireAt time.Time
}

const listDueOneShotSQL = `
SELECT id, project_id, kind, status, generation, run_id, policy_digest,
       fire_at, entity_version, as_of_revision, body
FROM schedules WHERE status = 'armed' AND fire_at IS NOT NULL AND fire_at <= ? ORDER BY id`

const listDueRecurringSQL = `
SELECT s.id, s.project_id, s.kind, s.status, s.generation,
       s.run_id, s.policy_digest, s.fire_at,
       s.entity_version, s.as_of_revision, s.body, t.generation, t.next_nominal_fire_at
FROM schedules s JOIN schedule_timers t ON t.schedule_id = s.id
WHERE s.status = 'armed' AND t.next_nominal_fire_at <= ? ORDER BY s.id`

// ListDueSchedules returns every armed schedule whose nominal fire instant
// is at or before now: one-shot deadlines by their extracted fire_at,
// recurring schedules by their timer row. A timer row whose generation does
// not match the aggregate is skipped: it is a stale clock a re-arm is about
// to replace, not a due fire.
func (tx *ReadTx) ListDueSchedules(ctx context.Context, now time.Time) ([]DueSchedule, error) {
	var due []DueSchedule
	rows, err := tx.tx.QueryContext(ctx, listDueOneShotSQL, now.UTC().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list due schedules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		schedule, _, err := tx.scanScheduleSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("list due schedules: %w", err)
		}
		due = append(due, DueSchedule{Schedule: schedule, NominalFireAt: *schedule.FireAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list due schedules: %w", err)
	}

	recurring, err := tx.tx.QueryContext(ctx, listDueRecurringSQL, now.UTC().UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list due schedules: %w", err)
	}
	defer func() { _ = recurring.Close() }()
	for recurring.Next() {
		var (
			id              string
			projectID       string
			kind            string
			status          string
			generation      int64
			runID           sql.NullString
			policy          sql.NullString
			fireAt          sql.NullInt64
			snap            Snapshot
			body            []byte
			timerGeneration int64
			nextNominal     int64
		)
		if err := recurring.Scan(&id, &projectID, &kind, &status, &generation, &runID, &policy, &fireAt,
			&snap.EntityVersion, &snap.AsOfRevision, &body, &timerGeneration, &nextNominal); err != nil {
			return nil, fmt.Errorf("list due schedules: %w", err)
		}
		schedule, _, err := tx.scanScheduleSnapshot(fixedRow{
			id, projectID, kind, status, generation, runID, policy, fireAt,
			snap.EntityVersion, snap.AsOfRevision, body,
		})
		if err != nil {
			return nil, fmt.Errorf("list due schedules: %w", err)
		}
		if timerGeneration != schedule.Generation {
			continue
		}
		due = append(due, DueSchedule{
			Schedule:      schedule,
			NominalFireAt: time.Unix(0, nextNominal).UTC(),
		})
	}
	if err := recurring.Err(); err != nil {
		return nil, fmt.Errorf("list due schedules: %w", err)
	}
	return due, nil
}

// fixedRow adapts already-scanned schedule columns back onto the shared
// reconstruction sequence, so the recurring due scan cannot skip a gate the
// single Get runs.
type fixedRow struct {
	id            string
	projectID     string
	kind          string
	status        string
	generation    int64
	runID         sql.NullString
	policy        sql.NullString
	fireAt        sql.NullInt64
	entityVersion int64
	asOfRevision  int64
	body          []byte
}

func (r fixedRow) Scan(dest ...any) error {
	if len(dest) != 11 {
		return fmt.Errorf("fixed schedule row scans 11 columns, got %d", len(dest))
	}
	*dest[0].(*string) = r.id
	*dest[1].(*string) = r.projectID
	*dest[2].(*string) = r.kind
	*dest[3].(*string) = r.status
	*dest[4].(*int64) = r.generation
	*dest[5].(*sql.NullString) = r.runID
	*dest[6].(*sql.NullString) = r.policy
	*dest[7].(*sql.NullInt64) = r.fireAt
	*dest[8].(*int64) = r.entityVersion
	*dest[9].(*int64) = r.asOfRevision
	*dest[10].(*[]byte) = r.body
	return nil
}

const setScheduleTimerSQL = `
INSERT INTO schedule_timers (schedule_id, generation, next_nominal_fire_at)
VALUES (?, ?, ?)
ON CONFLICT (schedule_id) DO UPDATE SET
    generation           = excluded.generation,
    next_nominal_fire_at = excluded.next_nominal_fire_at`

// SetScheduleTimer records a recurring schedule's next nominal fire
// instant, replacing any previous clock (including an older generation's).
func (tx *InternalTx) SetScheduleTimer(
	ctx context.Context, id domain.ScheduleID, generation int64, next time.Time,
) error {
	if id == "" {
		return errors.New("set schedule timer: empty schedule id")
	}
	if generation < 1 {
		return fmt.Errorf("set schedule timer %q: generation %d", id, generation)
	}
	if _, err := tx.tx.ExecContext(ctx, setScheduleTimerSQL,
		id, generation, next.UTC().UnixNano()); err != nil {
		return fmt.Errorf("set schedule timer %q: %w", id, err)
	}
	return nil
}

// DeleteScheduleTimer removes a schedule's clock; deleting a missing row is
// not an error (a terminated one-shot never had one).
func (tx *InternalTx) DeleteScheduleTimer(ctx context.Context, id domain.ScheduleID) error {
	if id == "" {
		return errors.New("delete schedule timer: empty schedule id")
	}
	if _, err := tx.tx.ExecContext(ctx,
		`DELETE FROM schedule_timers WHERE schedule_id = ?`, id); err != nil {
		return fmt.Errorf("delete schedule timer %q: %w", id, err)
	}
	return nil
}

// GetScheduleTimer reads a schedule's clock; ok reports whether one exists.
func (tx *ReadTx) GetScheduleTimer(
	ctx context.Context, id domain.ScheduleID,
) (generation int64, next time.Time, ok bool, err error) {
	if id == "" {
		return 0, time.Time{}, false, errors.New("get schedule timer: empty schedule id")
	}
	var nextNano int64
	scanErr := tx.tx.QueryRowContext(ctx,
		`SELECT generation, next_nominal_fire_at FROM schedule_timers WHERE schedule_id = ?`, id).
		Scan(&generation, &nextNano)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if scanErr != nil {
		return 0, time.Time{}, false, fmt.Errorf("get schedule timer %q: %w", id, scanErr)
	}
	return generation, time.Unix(0, nextNano).UTC(), true, nil
}

const createOccurrenceSQL = `
INSERT INTO schedule_occurrences
    (schedule_id, generation, nominal_fire_at, status, gap_missed, gap_earliest, created_at)
VALUES (?, ?, ?, 'pending', ?, ?, ?)
ON CONFLICT (schedule_id, generation, nominal_fire_at) DO NOTHING`

// CreatePendingScheduleOccurrence commits one pending occurrence under its
// identity (§5.16). A duplicate identity inserts nothing and reports
// created false: a crash-retried fire converges on the original pending
// row, which stays the durably redeliverable carrier.
func (tx *InternalTx) CreatePendingScheduleOccurrence(
	ctx context.Context, occ domain.ScheduleOccurrence,
) (bool, error) {
	if err := occ.Validate(); err != nil {
		return false, fmt.Errorf("create schedule occurrence: %w", err)
	}
	if occ.Status != domain.OccurrencePending {
		return false, fmt.Errorf("create schedule occurrence %q: status %q",
			occ.ScheduleID, occ.Status)
	}
	var gapMissed, gapEarliest any
	if occ.Gap != nil {
		gapMissed = occ.Gap.MissedOccurrences
		gapEarliest = occ.Gap.EarliestMissedAt.UnixNano()
	}
	res, err := tx.tx.ExecContext(ctx, createOccurrenceSQL,
		occ.ScheduleID, occ.Generation, occ.NominalFireAt.UnixNano(),
		gapMissed, gapEarliest, occ.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("create schedule occurrence %q: %w", occ.ScheduleID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("create schedule occurrence %q: %w", occ.ScheduleID, err)
	}
	return inserted > 0, nil
}

// scanOccurrence reconstructs one occurrence row and re-validates it: the
// deserialization backstop for rows that bypassed the domain constructor.
func scanOccurrence(sc scanner) (domain.ScheduleOccurrence, error) {
	var (
		scheduleID  string
		generation  int64
		nominalNano int64
		status      string
		gapMissed   sql.NullInt64
		gapEarliest sql.NullInt64
		createdAt   string
		consumedAt  sql.NullString
		outcome     sql.NullString
	)
	if err := sc.Scan(&scheduleID, &generation, &nominalNano, &status,
		&gapMissed, &gapEarliest, &createdAt, &consumedAt, &outcome); err != nil {
		return domain.ScheduleOccurrence{}, err
	}
	occ := domain.ScheduleOccurrence{
		ScheduleID:    domain.ScheduleID(scheduleID),
		Generation:    generation,
		NominalFireAt: time.Unix(0, nominalNano).UTC(),
		Status:        domain.ScheduleOccurrenceStatus(status),
	}
	if gapMissed.Valid != gapEarliest.Valid {
		return domain.ScheduleOccurrence{}, errRowInconsistent
	}
	if gapMissed.Valid {
		occ.Gap = &domain.ScheduleFireGap{
			MissedOccurrences: gapMissed.Int64,
			EarliestMissedAt:  time.Unix(0, gapEarliest.Int64).UTC(),
		}
	}
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.ScheduleOccurrence{}, fmt.Errorf("stored created_at invalid: %w", err)
	}
	occ.CreatedAt = created.UTC()
	if consumedAt.Valid {
		consumed, err := time.Parse(time.RFC3339Nano, consumedAt.String)
		if err != nil {
			return domain.ScheduleOccurrence{}, fmt.Errorf("stored consumed_at invalid: %w", err)
		}
		consumedUTC := consumed.UTC()
		occ.ConsumedAt = &consumedUTC
	}
	if outcome.Valid {
		out := domain.ScheduleOccurrenceOutcome(outcome.String)
		occ.Outcome = &out
	}
	if err := occ.Validate(); err != nil {
		return domain.ScheduleOccurrence{}, fmt.Errorf("stored row invalid: %w", err)
	}
	return occ, nil
}

const occurrenceColumns = `
schedule_id, generation, nominal_fire_at, status, gap_missed, gap_earliest,
created_at, consumed_at, outcome`

const listPendingOccurrencesSQL = `
SELECT ` + occurrenceColumns + `
FROM schedule_occurrences WHERE status = 'pending' ORDER BY id`

// ListPendingScheduleOccurrences returns every pending occurrence in
// creation order: the redelivery scan after a restart or a failed
// consumption (§5.16).
func (tx *ReadTx) ListPendingScheduleOccurrences(ctx context.Context) ([]domain.ScheduleOccurrence, error) {
	rows, err := tx.tx.QueryContext(ctx, listPendingOccurrencesSQL)
	if err != nil {
		return nil, fmt.Errorf("list pending schedule occurrences: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.ScheduleOccurrence
	for rows.Next() {
		occ, err := scanOccurrence(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending schedule occurrences: %w", err)
		}
		out = append(out, occ)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending schedule occurrences: %w", err)
	}
	return out, nil
}

const getOccurrenceSQL = `
SELECT ` + occurrenceColumns + `
FROM schedule_occurrences WHERE schedule_id = ? AND generation = ? AND nominal_fire_at = ?`

// GetScheduleOccurrence returns one occurrence by its identity.
func (tx *ReadTx) GetScheduleOccurrence(
	ctx context.Context, id domain.ScheduleID, generation int64, nominalFireAt time.Time,
) (domain.ScheduleOccurrence, error) {
	occ, err := scanOccurrence(tx.tx.QueryRowContext(ctx, getOccurrenceSQL,
		id, generation, nominalFireAt.UTC().UnixNano()))
	if err != nil {
		return domain.ScheduleOccurrence{}, fmt.Errorf("get schedule occurrence %q: %w", id, notFoundOr(err))
	}
	return occ, nil
}

const consumeOccurrenceSQL = `
UPDATE schedule_occurrences SET status = 'consumed', consumed_at = ?, outcome = ?
WHERE schedule_id = ? AND generation = ? AND nominal_fire_at = ? AND status = 'pending'`

// ConsumeScheduleOccurrence flips one pending occurrence to consumed with
// its outcome, guarded on pending status so it cannot land twice. It
// reports whether this call consumed the row; the caller commits it in the
// same transaction as the handler's durable outcome, which is what makes
// consumption-and-outcome atomic (§5.16) — a rolled-back transaction
// leaves the row pending for redelivery.
func (tx *InternalTx) ConsumeScheduleOccurrence(
	ctx context.Context,
	id domain.ScheduleID, generation int64, nominalFireAt time.Time,
	outcome domain.ScheduleOccurrenceOutcome, consumedAt time.Time,
) (bool, error) {
	if id == "" {
		return false, errors.New("consume schedule occurrence: empty schedule id")
	}
	consumedUTC := consumedAt.UTC()
	occ := domain.ScheduleOccurrence{
		ScheduleID: id, Generation: generation,
		NominalFireAt: nominalFireAt.UTC(), Status: domain.OccurrenceConsumed,
		CreatedAt:  consumedUTC,
		ConsumedAt: &consumedUTC, Outcome: &outcome,
	}
	// Validate the write-shape (outcome membership, UTC, identity) before
	// touching the row; CreatedAt above only satisfies the shape check and
	// is not written.
	if err := occ.Validate(); err != nil {
		return false, fmt.Errorf("consume schedule occurrence %q: %w", id, err)
	}
	res, err := tx.tx.ExecContext(ctx, consumeOccurrenceSQL,
		consumedAt.UTC().Format(time.RFC3339Nano), outcome,
		id, generation, nominalFireAt.UTC().UnixNano())
	if err != nil {
		return false, fmt.Errorf("consume schedule occurrence %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("consume schedule occurrence %q: %w", id, err)
	}
	return affected > 0, nil
}
