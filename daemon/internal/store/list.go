package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Snapshotted pairs one reconstructed entity with its store-stamped §5.14
// sync metadata: the element type of the collection reads below. The single
// Gets return the pair as two values; a list element forces a pairing type.
type Snapshotted[T any] struct {
	Value    T
	Snapshot Snapshot
}

// The List methods enumerate every persisted row of one synchronized
// aggregate for the §5.14 /sync/bootstrap snapshot: called (with ServerState)
// inside one Store.Read callback, every collection is read at the same SQLite
// snapshot. Shared semantics:
//
//   - Order is deterministic: ascending primary key under SQLite's default
//     BINARY collation (byte order) — id for runs, conversations, and
//     attention items; (item_id, device_id, channel, attempt) for
//     deliveries — never insertion or row order.
//   - Every row reconstructs through the same scan function its single Get
//     uses (see scanner), so the trust-boundary gates are identical; one
//     forged or corrupt row fails the whole list closed rather than being
//     skipped, since a bootstrap must never silently omit state.
//   - Results are materialized: collections are local-daemon scale, so
//     whole-table slices beat an iterator's ceremony. Pagination is the sync
//     surface's concern (#66), not the store's. An empty collection is an
//     empty, non-nil slice, so a direct JSON projection emits [] not null.

const listRunsSQL = `
SELECT id, project_id, policy_digest, entity_version, as_of_revision, body
FROM runs ORDER BY id`

// ListRuns enumerates every persisted run (List semantics above).
func (tx *ReadTx) ListRuns(ctx context.Context) ([]Snapshotted[domain.Run], error) {
	runs, err := listSnapshotted(ctx, tx, listRunsSQL, (*ReadTx).scanRunSnapshot)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, nil
}

const listConversationsSQL = `
SELECT id, entity_version, as_of_revision, body
FROM conversations ORDER BY id`

// ListConversations enumerates every persisted conversation (List semantics
// above).
func (tx *ReadTx) ListConversations(ctx context.Context) ([]Snapshotted[domain.Conversation], error) {
	conversations, err := listSnapshotted(ctx, tx, listConversationsSQL, (*ReadTx).scanConversationSnapshot)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	return conversations, nil
}

const listAttentionItemsSQL = `
SELECT id, project_id, conversation_id, entity_version, as_of_revision, body
FROM attention_items ORDER BY id`

// ListAttentionItems enumerates every persisted attention item (List
// semantics above), re-running the evidence gate on each row like
// GetAttentionItemSnapshot.
func (tx *ReadTx) ListAttentionItems(ctx context.Context) ([]Snapshotted[domain.AttentionItem], error) {
	items, err := listSnapshotted(ctx, tx, listAttentionItemsSQL, (*ReadTx).scanAttentionItemSnapshot)
	if err != nil {
		return nil, fmt.Errorf("list attention items: %w", err)
	}
	return items, nil
}

const listAttentionDeliveriesSQL = `
SELECT item_id, device_id, channel, attempt, entity_version, as_of_revision, body
FROM attention_deliveries ORDER BY item_id, device_id, channel, attempt`

// ListAttentionDeliveries enumerates every persisted attention delivery (List
// semantics above).
func (tx *ReadTx) ListAttentionDeliveries(ctx context.Context) ([]Snapshotted[domain.AttentionDelivery], error) {
	deliveries, err := listSnapshotted(ctx, tx, listAttentionDeliveriesSQL, (*ReadTx).scanAttentionDeliverySnapshot)
	if err != nil {
		return nil, fmt.Errorf("list attention deliveries: %w", err)
	}
	return deliveries, nil
}

// listSnapshotted runs one constant list query and reconstructs each row
// through the entity's shared scan function (passed as a method expression,
// so the List cannot skip a gate the Get runs).
func listSnapshotted[T any](ctx context.Context, tx *ReadTx, query string, scan func(*ReadTx, scanner) (T, Snapshot, error)) ([]Snapshotted[T], error) {
	rows, err := tx.tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	// Empty enumerations are empty, never nil: the results feed a wire
	// projection, where a nil slice marshals as JSON null while the sync
	// schemas require arrays.
	out := []Snapshotted[T]{}
	for rows.Next() {
		value, snap, err := scan(tx, rows)
		if err != nil {
			// The scan functions return bare errors; the enumeration position
			// (1-based, stable under the documented key order) locates the
			// corrupt row, which a whole-table read has no single key to name.
			return nil, fmt.Errorf("row %d: %w", len(out)+1, err)
		}
		out = append(out, Snapshotted[T]{Value: value, Snapshot: snap})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
