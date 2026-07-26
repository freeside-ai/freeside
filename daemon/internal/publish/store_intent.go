package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// commitReservedIntent is the one place in this package that hands a
// publication intent to the store's outbox. Both store-backed writers — the
// standalone ledger (store_ledger.go) and the composed decision transaction
// (store_decision.go) — route through it, so the checks that stand between a
// caller and a committed intent cannot drift apart between them, and a third
// writer inherits them by construction.
//
// It runs inside the caller's transaction: the standalone ledger opens one
// around it, the decision boundary composes it into the transaction that also
// records the fresh audit and resolves the authorization.
//
// The returned payload is the one durably held under key: the given payload
// when this call inserted it (recorded true), or the pre-existing row's payload
// when a prior attempt already committed the key (recorded false), so a retry
// converges on the original intent instead of re-recording.
func commitReservedIntent(
	ctx context.Context,
	tx *store.InternalTx,
	key, kind string,
	payload []byte,
) (prior []byte, recorded bool, err error) {
	entry, inserted, err := tx.EnqueueOutbox(ctx, key, kind, payload)
	if err != nil {
		return nil, false, err
	}
	// The outbox is unique by idempotency key alone, so a foreign row can
	// occupy this key under another kind. The returned row is the durable
	// intent this call is attesting to; verify both coordinates before allowing
	// an insert to commit or an existing row to converge.
	if entry.IdempotencyKey != key || entry.Kind != kind {
		return nil, false, fmt.Errorf("key %q holds kind %q", entry.IdempotencyKey, entry.Kind)
	}
	if entry.Quarantined() {
		return nil, false, fmt.Errorf("intent %q is quarantined: %w", key, ErrUnauthorizedPublication)
	}
	return entry.Payload, inserted, nil
}

// isPublicationRefusal reports whether an error from commitReservedIntent is a
// refusal of this publication rather than a failure of the transaction that
// carries it. The composed decision boundary commits the fresh audit it
// recorded before reporting one of these, since the GitHub observation behind
// that audit really happened.
func isPublicationRefusal(err error) bool {
	return errors.Is(err, ErrUnauthorizedPublication)
}
