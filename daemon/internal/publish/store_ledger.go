package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// StoreLedger is the store-backed IntentLedger: it commits each
// publication intent onto the store-owned outbox (plan §5.9) through the
// store's EnqueueOutbox, mirroring the StoreRecorder audit adapter
// (audit.go). Like that adapter it commits in its own internal
// transaction, invisible to client sync.
//
// This is the standalone-transaction form the publish lane needs now for
// the recovery scan and its kill tests (issue #82). The IntentLedger doc
// (ledger.go) reserves the production form for the Wave 2 engine, where
// the intent write rides the same Write transaction that commits the
// workflow decision the effect belongs to (§5.14); composing that
// transaction is the engine's, not this package's. The narrowing —
// building the standalone adapter now while leaving the engine-composed
// placement to Wave 2 — is recorded in this unit's decision note.
type StoreLedger struct {
	store *store.Store
}

// NewStoreLedger wires the ledger to an open store; a nil store fails
// closed at construction rather than at the first Record.
func NewStoreLedger(s *store.Store) (*StoreLedger, error) {
	if s == nil {
		return nil, errors.New("ledger: nil store")
	}
	return &StoreLedger{store: s}, nil
}

// Record enqueues the intent under key in its own internal transaction
// and reports the payload durably held there (the store's insert-or-
// converge contract, outbox.go): the given payload when this call
// inserted it (recorded true), or the pre-existing row's payload when a
// prior attempt already committed the key (recorded false), so a retry
// converges on the original intent instead of re-recording. The intent
// write precedes any external effect, so the caller's context governs it
// directly — a cancellation before commit safely abandons a publication
// nothing has dispatched yet.
func (l *StoreLedger) Record(ctx context.Context, key, kind string, payload []byte) (prior []byte, recorded bool, err error) {
	var (
		stored   []byte
		inserted bool
	)
	err = l.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		entry, ins, err := tx.EnqueueOutbox(ctx, key, kind, payload)
		if err != nil {
			return err
		}
		// The outbox is unique by idempotency key alone, so a foreign row
		// can occupy this key under another kind. The returned row is the
		// durable intent Record is attesting to; verify both coordinates
		// before allowing an insert to commit or an existing row to
		// converge.
		if entry.IdempotencyKey != key || entry.Kind != kind {
			return fmt.Errorf("key %q holds kind %q", entry.IdempotencyKey, entry.Kind)
		}
		stored, inserted = entry.Payload, ins
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("ledger: record intent %q: %w", key, err)
	}
	return stored, inserted, nil
}

// StoreLedger must satisfy the port it backs.
var _ IntentLedger = (*StoreLedger)(nil)
