package store

import (
	"context"
	"errors"
	"fmt"
	"time"
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
	Status         string
	CreatedAt      time.Time
}

// Outbox row statuses. Pending is the schema default at enqueue; dispatched
// records that the intent was handed to its provider, whose own durable
// intent record is the correctness dedup — the mark only bounds rescans
// (plan §5.14 discuss recovery).
const (
	outboxStatusPending    = "pending"
	outboxStatusDispatched = "dispatched"
)

const (
	enqueueOutboxSQL = `
INSERT INTO outbox (idempotency_key, kind, payload, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (idempotency_key) DO NOTHING`
	selectOutboxSQL = `
SELECT id, idempotency_key, kind, payload, status, created_at
FROM outbox WHERE idempotency_key = ?`
	listPendingOutboxSQL = `
SELECT id, idempotency_key, kind, payload, status, created_at
FROM outbox WHERE kind = ? AND status = ? ORDER BY id`
	markOutboxDispatchedSQL = `
UPDATE outbox SET status = ? WHERE idempotency_key = ? AND status = ?`

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
	entry, inserted, err := tx.record(ctx, enqueueOutboxSQL, selectOutboxSQL, key, kind, payload)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("enqueue outbox %q: %w", key, err)
	}
	return entry, inserted, nil
}

// ListPendingOutbox returns the committed-but-undispatched intents of one
// kind in insertion order: the recovery scan after a restart (§5.14 test 5
// "discuss commits and the daemon dies pre-invocation"). Dispatch then
// re-hands each to its provider, whose durable intent record dedups a repeat.
func (tx *ReadTx) ListPendingOutbox(ctx context.Context, kind string) ([]QueueEntry, error) {
	if kind == "" {
		return nil, errors.New("list pending outbox: empty kind")
	}
	rows, err := tx.tx.QueryContext(ctx, listPendingOutboxSQL, kind, outboxStatusPending)
	if err != nil {
		return nil, fmt.Errorf("list pending outbox %q: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	var entries []QueueEntry
	for rows.Next() {
		var (
			entry  QueueEntry
			stored string
		)
		if err := rows.Scan(&entry.ID, &entry.IdempotencyKey, &entry.Kind, &entry.Payload, &entry.Status, &stored); err != nil {
			return nil, fmt.Errorf("list pending outbox %q: %w", kind, err)
		}
		entry.CreatedAt, err = time.Parse(time.RFC3339Nano, stored)
		if err != nil {
			return nil, fmt.Errorf("list pending outbox %q: stored created_at invalid: %w", kind, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending outbox %q: %w", kind, err)
	}
	return entries, nil
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
	if _, err := tx.tx.ExecContext(ctx, markOutboxDispatchedSQL, outboxStatusDispatched, key, outboxStatusPending); err != nil {
		return fmt.Errorf("mark outbox dispatched %q: %w", key, err)
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
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
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
	entry.CreatedAt, err = time.Parse(time.RFC3339Nano, stored)
	if err != nil {
		return QueueEntry{}, false, fmt.Errorf("stored created_at invalid: %w", err)
	}
	return entry, affected > 0, nil
}
