package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
// A claim is the caller's proof that it holds the reservation occupying the
// key; a nil claim can only commit an unreserved invocation.
func commitReservedIntent(
	ctx context.Context,
	tx *store.InternalTx,
	key, kind string,
	payload []byte,
	claim *Reservation,
) (prior []byte, recorded bool, err error) {
	if kind == IntentKindPublication {
		var marker struct {
			ProducingInvocationID domain.InvocationID `json:"producing_invocation_id"`
		}
		if err := json.Unmarshal(payload, &marker); err == nil &&
			marker.ProducingInvocationID != "" {
			settling, err := DecodeIntent(payload)
			if err != nil {
				return nil, false, fmt.Errorf("decode execution publication intent %q: %w", key, err)
			}
			if claim == nil || settling.ReservationRunID != claim.RunID {
				return nil, false, fmt.Errorf(
					"execution publication intent %q has no matching reservation owner: %w",
					key, ErrInvocationReserved,
				)
			}
			if _, err := validateExecutionReservation(
				ctx, tx, key, claim, settling.ProducingInvocationID,
			); err != nil {
				return nil, false, err
			}
		}
	}
	entry, inserted, err := tx.EnqueueOutbox(ctx, key, kind, payload)
	if err != nil {
		return nil, false, err
	}
	// The outbox is unique by idempotency key alone, so a foreign row can
	// occupy this key under another kind. The returned row is the durable
	// intent this call is attesting to; verify both coordinates before allowing
	// an insert to commit or an existing row to converge.
	if entry.IdempotencyKey != key {
		return nil, false, fmt.Errorf("key %q read back key %q", key, entry.IdempotencyKey)
	}
	if entry.Quarantined() {
		return nil, false, fmt.Errorf("intent %q is quarantined: %w", key, ErrUnauthorizedPublication)
	}
	switch {
	case entry.Kind == kind:
		return entry.Payload, inserted, nil
	case entry.Kind == IntentKindReservation && kind == IntentKindPublication:
		// The row this call converged on is the reservation that was holding
		// this key for its owner. Committing the intent is that reservation
		// being settled, not a second row being written beside it.
		return promoteReservedIntent(ctx, tx, key, payload, entry, claim)
	}
	return nil, false, fmt.Errorf("key %q holds kind %q", entry.IdempotencyKey, entry.Kind)
}

// promoteReservedIntent settles a reservation into the intent it was holding
// the key for. The caller must present the claim that reserved it: without one,
// or with another owner's, the invocation stays reserved and nothing is
// written, which is the whole point of having occupied the key.
func promoteReservedIntent(
	ctx context.Context,
	tx *store.InternalTx,
	key string,
	payload []byte,
	entry store.QueueEntry,
	claim *Reservation,
) (prior []byte, recorded bool, err error) {
	// The payload is opaque to the store, so the reservation is a
	// reconstruction boundary: decode and re-validate it before letting it
	// decide who may write here.
	held, err := DecodeReservation(entry.Payload)
	if err != nil {
		return nil, false, fmt.Errorf("reserved intent %q: %w", key, err)
	}
	if claim == nil || !held.Same(*claim) {
		return nil, false, fmt.Errorf(
			"publication invocation %q is reserved by run %q: %w",
			key, held.RunID, ErrInvocationReserved,
		)
	}
	// A claim that matches the row but names another key would settle a
	// reservation the caller never made.
	claimKey, err := claim.Key()
	if err != nil {
		return nil, false, err
	}
	if claimKey != key {
		return nil, false, fmt.Errorf(
			"reservation claim for %q presented at %q", claimKey, key)
	}
	// The payload replacing the reservation must name the invocation whose key
	// it will occupy. An intent settled under another invocation's key would
	// fail the drain's own key-versus-payload check on every pass, so the row
	// could never drain — a permanent wedge, which is the failure class this
	// reservation exists to prevent. The read side already refuses such a row
	// (classifyInvocation); the write side must not create one.
	settling, err := DecodeIntent(payload)
	if err != nil {
		return nil, false, fmt.Errorf("settle reserved intent %q: %w", key, err)
	}
	if settling.InvocationID != claim.InvocationID {
		return nil, false, fmt.Errorf(
			"settling intent at %q names invocation %q: %w",
			key, settling.InvocationID, domain.ErrParentKeyMismatch,
		)
	}
	settled, promoted, err := tx.PromoteOutbox(
		ctx, key, IntentKindReservation, IntentKindPublication, entry.Payload, payload,
	)
	if err != nil {
		return nil, false, fmt.Errorf("settle reserved intent %q: %w", key, err)
	}
	if !promoted {
		// The row moved between reading it and settling it. Only the caller's
		// own already-settled intent is an acceptable outcome; anything else is
		// a write this call must not report as its own.
		if settled.Kind != IntentKindPublication || !bytes.Equal(settled.Payload, payload) {
			return nil, false, fmt.Errorf(
				"reserved intent %q settled as kind %q: %w",
				key, settled.Kind, ErrPublicationConflict,
			)
		}
		return settled.Payload, false, nil
	}
	return settled.Payload, true, nil
}

// isPublicationRefusal reports whether an error from commitReservedIntent is a
// refusal of this publication rather than a failure of the transaction that
// carries it. The composed decision boundary commits the fresh audit it
// recorded before reporting one of these, since the GitHub observation behind
// that audit really happened. Neither refusal writes anything itself.
func isPublicationRefusal(err error) bool {
	return errors.Is(err, ErrUnauthorizedPublication) || errors.Is(err, ErrInvocationReserved)
}
