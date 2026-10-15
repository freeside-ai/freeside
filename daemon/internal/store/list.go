package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

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
SELECT id, project_id, task_id, policy_digest, campaign_id, attempt_number, attempt_reason,
       parent_run_id, entity_version, as_of_revision, body
FROM runs ORDER BY id`

// ListRuns enumerates every persisted run (List semantics above).
func (tx *ReadTx) ListRuns(ctx context.Context) ([]Snapshotted[domain.Run], error) {
	runs, err := listSnapshotted(ctx, tx, listRunsSQL,
		func(tx *ReadTx, sc scanner) (domain.Run, Snapshot, error) {
			return tx.scanRunSnapshot(ctx, sc)
		})
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
SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id, subject_task_id, readiness_summary, readiness_detail, yield_history, entity_version, as_of_revision, body
FROM attention_items ORDER BY id`

// ListAttentionItems enumerates every persisted attention item (List
// semantics above), re-running the evidence gate on each row like
// GetAttentionItemSnapshot.
func (tx *ReadTx) ListAttentionItems(ctx context.Context) ([]Snapshotted[domain.AttentionItem], error) {
	items, err := listSnapshotted(ctx, tx, listAttentionItemsSQL,
		func(tx *ReadTx, sc scanner) (domain.AttentionItem, Snapshot, error) {
			return tx.scanAttentionItemSnapshot(ctx, sc)
		})
	if err != nil {
		return nil, fmt.Errorf("list attention items: %w", err)
	}
	return items, nil
}

// AttentionItemRow is one attention_items row as a diagnostic read returns
// it: either the reconstructed item or the error that refused the row.
//
// A refused row's ID and subject columns are the row's own extracted
// columns. Reconstruction may have failed before or at the check that binds
// them to the canonical body, so they are not trusted state: a caller may
// use them only to say which row failed and to route the failure to a task
// or run, never as facts about the item.
type AttentionItemRow struct {
	ID            domain.ItemID
	SubjectTaskID *domain.TaskID
	SubjectRunID  *domain.RunID
	// Item is set exactly when Err is nil.
	Item     *domain.AttentionItem
	Snapshot Snapshot
	Err      error
}

// ListAttentionItemRows enumerates every persisted attention item in
// ListAttentionItems' order, reconstructing each row through the same gates,
// but reports a refused row in place instead of failing the list. It is for
// diagnostic records that must show what they can read; the sync bootstrap
// keeps ListAttentionItems, which never omits state. The returned error is
// the query's own failure; any error while reconstructing a row, a database
// error inside one of its gates included, is that row's Err.
func (tx *ReadTx) ListAttentionItemRows(ctx context.Context) ([]AttentionItemRow, error) {
	rows, err := tx.tx.QueryContext(ctx, listAttentionItemsSQL)
	if err != nil {
		return nil, fmt.Errorf("list attention item rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []AttentionItemRow{}
	for rows.Next() {
		columns, err := scanAttentionItemColumns(rows)
		if err != nil {
			return nil, fmt.Errorf("list attention item rows: row %d: %w", len(out)+1, err)
		}
		row := AttentionItemRow{ID: domain.ItemID(columns.id)}
		item, snap, err := tx.reconstructAttentionItemSnapshot(ctx, columns)
		if err != nil {
			row.Err = err
			if columns.subjectTaskID.Valid {
				row.SubjectTaskID = new(domain.TaskID(columns.subjectTaskID.String))
			}
			if columns.subjectRunID.Valid {
				row.SubjectRunID = new(domain.RunID(columns.subjectRunID.String))
			}
		} else {
			row.Item, row.Snapshot = &item, snap
			row.SubjectTaskID, row.SubjectRunID = item.Subject.TaskID, item.Subject.RunID
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list attention item rows: %w", err)
	}
	return out, nil
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

// ListCommandsForItem returns every immutable accepted command for one item
// in command-id order. Each row reconstructs through GetCommand, including its
// extracted-column and backup-binding gates.
func (tx *ReadTx) ListCommandsForItem(ctx context.Context, itemID domain.ItemID) ([]domain.Command, error) {
	if itemID == "" {
		return nil, fmt.Errorf("list commands for item: %w", domain.ErrEmptyID)
	}
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT command_id FROM commands WHERE item_id = ? ORDER BY command_id`, itemID)
	if err != nil {
		return nil, fmt.Errorf("list commands for item %q: %w", itemID, err)
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list commands for item %q: %w", itemID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list commands for item %q: %w", itemID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list commands for item %q: %w", itemID, err)
	}
	commands := make([]domain.Command, 0, len(ids))
	for _, id := range ids {
		command, err := tx.GetCommand(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("list commands for item %q: %w", itemID, err)
		}
		if command.ItemID != itemID {
			return nil, fmt.Errorf("list commands for item %q returned %q: %w",
				itemID, command.ItemID, errRowInconsistent)
		}
		commands = append(commands, command)
	}
	return commands, nil
}

// ListCommandsForActions returns every immutable accepted command whose
// action is in actions, in command-id order. It reconstructs each selected row
// through GetCommand, so the extracted columns and backup binding remain part
// of the read authority. Unrelated command and attention rows are not read.
func (tx *ReadTx) ListCommandsForActions(
	ctx context.Context, actions ...domain.Action,
) ([]domain.Command, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	for _, action := range actions {
		if !slices.Contains(domain.AllActions, action) {
			return nil, fmt.Errorf("list commands for actions: invalid action %q", action)
		}
	}
	actionSet, err := json.Marshal(actions)
	if err != nil {
		return nil, fmt.Errorf("list commands for actions: %w", err)
	}
	rows, err := tx.tx.QueryContext(ctx, `
SELECT command_id FROM commands
WHERE action IN (SELECT value FROM json_each(?))
ORDER BY command_id`, actionSet)
	if err != nil {
		return nil, fmt.Errorf("list commands for actions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list commands for actions: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list commands for actions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list commands for actions: %w", err)
	}
	commands := make([]domain.Command, 0, len(ids))
	for _, id := range ids {
		command, err := tx.GetCommand(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("list commands for actions: %w", err)
		}
		if !slices.Contains(actions, command.Action) {
			return nil, fmt.Errorf("list commands for actions returned %q: %w",
				command.Action, errRowInconsistent)
		}
		commands = append(commands, command)
	}
	return commands, nil
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
