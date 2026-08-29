package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// QueueEntry is one inbox or outbox row (§5.9): the two queues deliberately
// share a shape. Kind names the action or event type; Payload is opaque to
// the store. Status starts at "pending"; the dispatch loop itself lands with
// the engine, but the store owns the ledger reads and marks it drives
// (ListPendingOutbox, MarkOutboxDispatched).
type QueueEntry struct {
	ID             int64
	IdempotencyKey string
	Kind           string
	Payload        []byte
	PayloadVersion int
	PayloadDigest  string
	Status         string
	CreatedAt      time.Time
}

// Dispatched reports whether this durable queue row has completed its
// provider handoff.
func (e QueueEntry) Dispatched() bool {
	return e.Status == outboxStatusDispatched
}

// Quarantined reports whether a migration preserved this intent for audit
// while removing it from active recovery because its authority can no longer
// be reconstructed safely.
func (e QueueEntry) Quarantined() bool {
	return e.Status == outboxStatusQuarantined
}

func (e QueueEntry) validStatus() bool {
	return e.Status == outboxStatusPending ||
		e.Status == outboxStatusDispatching ||
		e.Status == outboxStatusDispatched ||
		e.Status == outboxStatusQuarantined
}

// Outbox row statuses. Pending is the schema default at enqueue; dispatched
// records that the intent was handed to its provider, whose own durable
// intent record is the correctness dedup — the mark only bounds rescans.
// Quarantined preserves an intent whose authority cannot be reconstructed
// while keeping it out of the pending recovery scan.
const (
	outboxStatusPending     = "pending"
	outboxStatusDispatching = "dispatching"
	outboxStatusDispatched  = "dispatched"
	outboxStatusQuarantined = "quarantined"
)

const (
	enqueueOutboxSQL = `
INSERT INTO outbox (idempotency_key, kind, payload, payload_version, payload_digest, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (idempotency_key) DO NOTHING`
	enqueueDispatchedOutboxSQL = `
INSERT INTO outbox (idempotency_key, kind, payload, payload_version, payload_digest, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (idempotency_key) DO NOTHING`
	selectOutboxSQL = `
SELECT id, idempotency_key, kind, payload, payload_version, payload_digest, status, created_at
FROM outbox WHERE idempotency_key = ?`
	listOutboxByStatusSQL = `
SELECT id, idempotency_key, kind, payload, payload_version, payload_digest, status, created_at
FROM outbox WHERE kind = ? AND status = ? ORDER BY id`
	listPendingOutboxSQL = `
SELECT id, idempotency_key, kind, payload, payload_version, payload_digest, status, created_at
FROM outbox WHERE kind = ? AND status IN (?, ?) ORDER BY id`
	markOutboxDispatchedSQL = `
	UPDATE outbox SET status = ?
WHERE idempotency_key = ? AND status IN (?, ?)`
	markOutboxDispatchingSQL = `
	UPDATE outbox SET status = ?
WHERE idempotency_key = ? AND status = ?`
	releaseOutboxDispatchSQL = `
UPDATE outbox SET status = ? WHERE idempotency_key = ? AND status = ?`
	promoteOutboxSQL = `
UPDATE outbox SET kind = ?, payload = ?, payload_version = ?, payload_digest = ?
WHERE idempotency_key = ? AND kind = ? AND payload = ? AND status = ?`

	recordInboxSQL = `
INSERT INTO inbox (idempotency_key, kind, payload, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (idempotency_key) DO NOTHING`
	selectInboxSQL = `
SELECT id, idempotency_key, kind, payload, status, created_at
FROM inbox WHERE idempotency_key = ?`
)

// EnqueueOutbox records the intent of an external effect under its
// idempotency key. A duplicate key returns the original row with inserted
// false and writes nothing, so a retried command converges on one intent.
// Call it inside the Write transaction that commits the decision the effect
// belongs to (§5.14 discuss semantics).
func (tx *InternalTx) EnqueueOutbox(ctx context.Context, key, kind string, payload []byte) (QueueEntry, bool, error) {
	entry, inserted, err := tx.recordOutbox(ctx, key, kind, payload)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("enqueue outbox %q: %w", key, err)
	}
	return entry, inserted, nil
}

// RecordDispatchedOutbox records a write-once internal fact outside every
// pending dispatch scan. It is deliberately available only on WriteTx: the
// enclosing client-visible transaction supplies the revision that makes a
// projection depending on the fact observable atomically with its outcome.
func (tx *WriteTx) RecordDispatchedOutbox(
	ctx context.Context, key, kind string, payload []byte,
) (QueueEntry, bool, error) {
	if key == "" {
		return QueueEntry{}, false, errors.New("record dispatched outbox: empty idempotency key")
	}
	if kind == "" {
		return QueueEntry{}, false, errors.New("record dispatched outbox: empty kind")
	}
	if payload == nil {
		payload = []byte{}
	}
	version := outboxPayloadVersion(kind)
	digest := contentaddr.Sum(payload)
	createdAt := formatTime(time.Now())
	res, err := tx.tx.ExecContext(ctx, enqueueDispatchedOutboxSQL,
		key, kind, payload, version, digest, outboxStatusDispatched, createdAt)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("record dispatched outbox %q: %w", key, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("record dispatched outbox %q: %w", key, err)
	}
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("record dispatched outbox %q: %w", key, err)
	}
	if entry.Status != outboxStatusDispatched {
		return QueueEntry{}, false, fmt.Errorf(
			"record dispatched outbox %q found status %q: %w",
			key, entry.Status, ErrImmutableConflict)
	}
	return entry, affected > 0, nil
}

// ListPendingOutbox returns the committed-but-undispatched intents of one
// kind in insertion order: the recovery scan after a restart (§5.14 test 5
// "discuss commits and the daemon dies pre-invocation"). Dispatch then
// re-hands each to its provider, whose durable intent record dedups a repeat.
func (tx *ReadTx) ListPendingOutbox(ctx context.Context, kind string) ([]QueueEntry, error) {
	return tx.listPendingOutbox(ctx, kind)
}

func (tx *ReadTx) listPendingOutbox(ctx context.Context, kind string) ([]QueueEntry, error) {
	if kind == "" {
		return nil, errors.New("list outbox: empty kind")
	}
	return tx.listOutboxQuery(ctx, listPendingOutboxSQL, kind, "pending or dispatching",
		kind, outboxStatusPending, outboxStatusDispatching)
}

// ListDispatchedOutbox returns completed intents of one kind in insertion
// order. Recovery uses it only when an upgrade must converge durable state
// owned by already-dispatched work; ordinary dispatch loops use
// ListPendingOutbox.
func (tx *ReadTx) ListDispatchedOutbox(ctx context.Context, kind string) ([]QueueEntry, error) {
	return tx.listOutboxByStatus(ctx, kind, outboxStatusDispatched)
}

// ListQuarantinedOutbox returns audit-preserved intents of one kind in
// insertion order. Quarantined work is never eligible for dispatch, but
// conservative ownership probes use these rows as evidence that a missing
// active claim is damaged state rather than permission to create competing
// work.
func (tx *ReadTx) ListQuarantinedOutbox(ctx context.Context, kind string) ([]QueueEntry, error) {
	return tx.listOutboxByStatus(ctx, kind, outboxStatusQuarantined)
}

func (tx *ReadTx) listOutboxByStatus(
	ctx context.Context,
	kind, status string,
) ([]QueueEntry, error) {
	if kind == "" {
		return nil, errors.New("list outbox: empty kind")
	}
	return tx.listOutboxQuery(ctx, listOutboxByStatusSQL, kind, status, kind, status)
}

func (tx *ReadTx) listOutboxQuery(ctx context.Context, query, kind, status string, args ...any) ([]QueueEntry, error) {
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list outbox %q status %q: %w", kind, status, err)
	}
	defer func() { _ = rows.Close() }()
	var entries []QueueEntry
	for rows.Next() {
		var (
			entry  QueueEntry
			stored string
		)
		if err := rows.Scan(
			&entry.ID, &entry.IdempotencyKey, &entry.Kind, &entry.Payload,
			&entry.PayloadVersion, &entry.PayloadDigest, &entry.Status, &stored,
		); err != nil {
			return nil, fmt.Errorf("list outbox %q status %q: %w", kind, status, err)
		}
		entry.CreatedAt, err = parseTime(stored)
		if err != nil {
			return nil, fmt.Errorf("list outbox %q status %q: stored created_at invalid: %w", kind, status, err)
		}
		if err := validateOutboxPayload(entry); err != nil {
			return nil, fmt.Errorf("list outbox %q status %q: %w", kind, status, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list outbox %q status %q: %w", kind, status, err)
	}
	return entries, nil
}

// GetOutbox returns one durable intent by idempotency key regardless of
// whether it is still pending or has been dispatched.
func (tx *ReadTx) GetOutbox(ctx context.Context, key string) (QueueEntry, error) {
	if key == "" {
		return QueueEntry{}, errors.New("get outbox: empty idempotency key")
	}
	var (
		entry  QueueEntry
		stored string
	)
	err := tx.tx.QueryRowContext(ctx, selectOutboxSQL, key).Scan(
		&entry.ID, &entry.IdempotencyKey, &entry.Kind, &entry.Payload,
		&entry.PayloadVersion, &entry.PayloadDigest, &entry.Status, &stored,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueEntry{}, fmt.Errorf("get outbox %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return QueueEntry{}, fmt.Errorf("get outbox %q: %w", key, err)
	}
	entry.CreatedAt, err = parseTime(stored)
	if err != nil {
		return QueueEntry{}, fmt.Errorf("get outbox %q: stored created_at invalid: %w", key, err)
	}
	if !entry.validStatus() {
		return QueueEntry{}, fmt.Errorf("get outbox %q: invalid status %q", key, entry.Status)
	}
	if err := validateOutboxPayload(entry); err != nil {
		return QueueEntry{}, fmt.Errorf("get outbox %q: %w", key, err)
	}
	return entry, nil
}

// GetInbox returns one durable accepted result by idempotency key.
func (tx *ReadTx) GetInbox(ctx context.Context, key string) (QueueEntry, error) {
	if key == "" {
		return QueueEntry{}, errors.New("get inbox: empty idempotency key")
	}
	var (
		entry  QueueEntry
		stored string
	)
	err := tx.tx.QueryRowContext(ctx, selectInboxSQL, key).Scan(
		&entry.ID, &entry.IdempotencyKey, &entry.Kind, &entry.Payload,
		&entry.Status, &stored,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueEntry{}, fmt.Errorf("get inbox %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return QueueEntry{}, fmt.Errorf("get inbox %q: %w", key, err)
	}
	entry.CreatedAt, err = parseTime(stored)
	if err != nil {
		return QueueEntry{}, fmt.Errorf("get inbox %q: stored created_at invalid: %w", key, err)
	}
	return entry, nil
}

// MarkOutboxDispatched flips a pending intent to dispatched. It is idempotent
// (marking a missing or already-dispatched key affects no row and is not an
// error) and dispatch bookkeeping is not client-visible, so it belongs inside
// WriteInternal — a re-dispatch after a crashed mark must not invalidate
// client caches. The provider's durable intent record, not this mark, is the
// effectively-once guard.
func (tx *InternalTx) MarkOutboxDispatched(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("mark outbox dispatched: empty idempotency key")
	}
	if _, err := tx.tx.ExecContext(ctx, markOutboxDispatchedSQL,
		outboxStatusDispatched, key, outboxStatusPending, outboxStatusDispatching); err != nil {
		return fmt.Errorf("mark outbox dispatched %q: %w", key, err)
	}
	return nil
}

// MarkOutboxDispatching reserves a pending intent before its provider handoff.
// Recovery lists this state with pending work, so a crash before Start remains
// retryable while an overlapping admission transaction sees the reservation.
func (tx *InternalTx) MarkOutboxDispatching(ctx context.Context, key string) error {
	_, err := tx.TryMarkOutboxDispatching(ctx, key)
	return err
}

// TryMarkOutboxDispatching claims a pending intent's pre-start reservation.
// Its changed result identifies the one dispatcher that may hand the intent to
// the driver or release the reservation after a synchronous refusal. A caller
// that observes false must leave the current owner alone: it may still be
// materializing inputs for this same invocation.
func (tx *InternalTx) TryMarkOutboxDispatching(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, errors.New("mark outbox dispatching: empty idempotency key")
	}
	result, err := tx.tx.ExecContext(ctx, markOutboxDispatchingSQL,
		outboxStatusDispatching, key, outboxStatusPending)
	if err != nil {
		return false, fmt.Errorf("mark outbox dispatching %q: %w", key, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark outbox dispatching %q rows affected: %w", key, err)
	}
	return changed == 1, nil
}

// ReleaseOutboxDispatch returns an unstarted reservation to pending after a
// synchronous driver refusal. It never reopens a driver-accepted handoff.
func (tx *InternalTx) ReleaseOutboxDispatch(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("release outbox dispatch: empty idempotency key")
	}
	if _, err := tx.tx.ExecContext(ctx, releaseOutboxDispatchSQL,
		outboxStatusPending, key, outboxStatusDispatching); err != nil {
		return fmt.Errorf("release outbox dispatch %q: %w", key, err)
	}
	return nil
}

// PromoteOutbox refines one pending row in place: a placeholder committed
// under fromKind becomes its final kind and payload, keeping id, created_at,
// and status. It exists so a caller can occupy an idempotency key before it
// knows the final payload — the key is the contested resource — and then
// settle it without the row ever being absent for a competing writer to take.
//
// The UPDATE is guarded on the exact current kind, payload, and pending
// status, so it cannot land on a row that moved underneath it. Affecting no
// row is not an error: a retried promotion is ordinary, so the current row is
// returned with promoted false and the caller decides whether what it found is
// what it wanted. The row is always re-read and re-checked before returning,
// since the guard proves what was matched, not what the row now holds.
func (tx *InternalTx) PromoteOutbox(
	ctx context.Context,
	key, fromKind, toKind string,
	expectPayload, payload []byte,
) (QueueEntry, bool, error) {
	if key == "" {
		return QueueEntry{}, false, errors.New("promote outbox: empty idempotency key")
	}
	if fromKind == "" || toKind == "" {
		return QueueEntry{}, false, fmt.Errorf("promote outbox %q: empty kind", key)
	}
	if fromKind == toKind {
		// A promotion that does not change the kind would let the guard match
		// the row it just wrote on a retry, so a payload rewrite could pass as
		// a fresh promotion. Payload-only edits are not this method's contract.
		return QueueEntry{}, false, fmt.Errorf("promote outbox %q: kind %q is unchanged", key, toKind)
	}
	if expectPayload == nil {
		expectPayload = []byte{}
	}
	if payload == nil {
		payload = []byte{}
	}
	version := outboxPayloadVersion(toKind)
	digest := contentaddr.Sum(payload)
	res, err := tx.tx.ExecContext(ctx, promoteOutboxSQL,
		toKind, payload, version, digest,
		key, fromKind, expectPayload, outboxStatusPending)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("promote outbox %q: %w", key, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("promote outbox %q: %w", key, err)
	}
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("promote outbox %q: %w", key, err)
	}
	if affected > 0 && (entry.Kind != toKind || !bytes.Equal(entry.Payload, payload) ||
		entry.Status != outboxStatusPending) {
		return QueueEntry{}, false, fmt.Errorf(
			"promote outbox %q: row holds kind %q after promotion", key, entry.Kind)
	}
	return entry, affected > 0, nil
}

func (tx *InternalTx) recordOutbox(
	ctx context.Context, key, kind string, payload []byte,
) (QueueEntry, bool, error) {
	if key == "" {
		return QueueEntry{}, false, errors.New("empty idempotency key")
	}
	if kind == "" {
		return QueueEntry{}, false, errors.New("empty kind")
	}
	if payload == nil {
		payload = []byte{}
	}
	version := outboxPayloadVersion(kind)
	digest := contentaddr.Sum(payload)
	createdAt := formatTime(time.Now())
	res, err := tx.tx.ExecContext(
		ctx, enqueueOutboxSQL, key, kind, payload, version, digest, createdAt,
	)
	if err != nil {
		return QueueEntry{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return QueueEntry{}, false, err
	}
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return QueueEntry{}, false, err
	}
	return entry, affected > 0, nil
}

func outboxPayloadVersion(kind string) int {
	if kind == readyPublicationIntentKind {
		return 2
	}
	return 1
}

func validateOutboxPayload(entry QueueEntry) error {
	if entry.PayloadVersion != 1 && entry.PayloadVersion != 2 {
		return fmt.Errorf("stored payload version %d is invalid", entry.PayloadVersion)
	}
	if !contentaddr.Valid(entry.PayloadDigest) {
		return fmt.Errorf("stored payload digest %q is invalid", entry.PayloadDigest)
	}
	if got := contentaddr.Sum(entry.Payload); got != entry.PayloadDigest {
		return fmt.Errorf(
			"stored payload digest %s, computed %s: %w",
			entry.PayloadDigest, got, domain.ErrParentKeyMismatch,
		)
	}
	return nil
}

// RecordInbox dedups an externally-triggered intake under its idempotency
// key, mirroring EnqueueOutbox. Intake bookkeeping is not client-visible; use
// it inside WriteInternal (or Write, when the same transaction also commits
// client-visible state).
func (tx *InternalTx) RecordInbox(ctx context.Context, key, kind string, payload []byte) (QueueEntry, bool, error) {
	entry, inserted, err := tx.record(ctx, recordInboxSQL, selectInboxSQL, key, kind, payload)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("record inbox %q: %w", key, err)
	}
	return entry, inserted, nil
}

func (tx *InternalTx) record(ctx context.Context, insertSQL, selectSQL, key, kind string, payload []byte) (QueueEntry, bool, error) {
	// An empty key would collapse unrelated actions onto one row; an empty
	// kind is unroutable. The schema CHECKs mirror these, but failing here
	// names the problem instead of surfacing a constraint error.
	if key == "" {
		return QueueEntry{}, false, errors.New("empty idempotency key")
	}
	if kind == "" {
		return QueueEntry{}, false, errors.New("empty kind")
	}
	if payload == nil {
		// A nil slice would bind as NULL and trip the NOT NULL constraint;
		// an intentionally empty payload is fine.
		payload = []byte{}
	}
	createdAt := formatTime(time.Now())
	res, err := tx.tx.ExecContext(ctx, insertSQL, key, kind, payload, createdAt)
	if err != nil {
		return QueueEntry{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return QueueEntry{}, false, err
	}

	var (
		entry  QueueEntry
		stored string
	)
	err = tx.tx.QueryRowContext(ctx, selectSQL, key).
		Scan(&entry.ID, &entry.IdempotencyKey, &entry.Kind, &entry.Payload, &entry.Status, &stored)
	if err != nil {
		return QueueEntry{}, false, err
	}
	entry.CreatedAt, err = parseTime(stored)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("stored created_at invalid: %w", err)
	}
	if !entry.validStatus() {
		return QueueEntry{}, false, fmt.Errorf("stored status %q is invalid", entry.Status)
	}
	return entry, affected > 0, nil
}
