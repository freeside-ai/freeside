package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// IntentKindReservation is the outbox kind a publication invocation is held
// under between the moment a workflow commits to publishing under it and the
// moment the candidate identity exists to publish. A reservation occupies the
// *publication intent's own key* (Reservation.Key), not a key beside it: the
// key is the contested resource, so holding it is what makes the reservation
// enforceable. Every writer that reaches that key through EnqueueOutbox finds
// a kind it did not expect and fails closed, whether or not it knows
// reservations exist.
const IntentKindReservation = "publish.invocation_reservation"

// reservationVersion is stamped into every reservation payload. A decoded row
// carrying another version is a shape this build cannot interpret, and an
// uninterpretable reservation must not read as an absent one.
const reservationVersion = "freeside-publication-reservation/v1"

// Reservation is the durable claim on one publication invocation ID, held from
// admission until the intent that replaces it. It exists because the invocation
// ID is immutable committed state: a workflow that has already durably bound
// itself to one cannot renegotiate, so a foreign intent arriving at that key
// before reconciliation would strand the workflow permanently.
//
// RunID is the whole ownership proof. It defends against a confused or buggy
// writer, not a hostile in-process caller: any code holding the run ID can
// present a matching claim. That is the honest boundary — publish enforces that
// two *different* owners cannot collide on one invocation, not that a caller is
// who it says it is.
type Reservation struct {
	Version      string              `json:"version"`
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        domain.RunID        `json:"run_id"`
}

// NewReservation builds the claim one run holds on one publication invocation.
func NewReservation(invocationID domain.InvocationID, runID domain.RunID) (Reservation, error) {
	r := Reservation{
		Version:      reservationVersion,
		InvocationID: invocationID,
		RunID:        runID,
	}
	if err := r.Validate(); err != nil {
		return Reservation{}, err
	}
	return r, nil
}

// Validate reports whether the reservation is well-formed. Like Intent.Validate
// it runs on both sides of the ledger boundary: before encoding, and on every
// decode, since a decoded outbox row is a reconstructed value and is not
// trusted to be well-formed.
func (r Reservation) Validate() error {
	if r.Version != reservationVersion {
		return fmt.Errorf("reservation: version %q is not %q", r.Version, reservationVersion)
	}
	if r.InvocationID == "" {
		return errors.New("reservation: empty invocation id")
	}
	// An empty run id would make every claim match every reservation, so the
	// ownership check would pass for a writer presenting nothing at all.
	if r.RunID == "" {
		return errors.New("reservation: empty run id")
	}
	return nil
}

// Encode validates and serializes the reservation for the outbox payload.
func (r Reservation) Encode() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("reservation: encode: %w", err)
	}
	return payload, nil
}

// DecodeReservation deserializes and validates an outbox payload. Unknown
// fields and trailing data fail closed: a reservation this package cannot fully
// interpret must not be treated as one it can, since the alternative reading is
// "the invocation is free".
func DecodeReservation(payload []byte) (Reservation, error) {
	var r Reservation
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Reservation{}, fmt.Errorf("reservation: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Reservation{}, errors.New("reservation: decode: trailing data after the reservation")
	}
	if err := r.Validate(); err != nil {
		return Reservation{}, err
	}
	return r, nil
}

// Key is the outbox key the reservation holds: the publication intent's own
// key, so the reservation and the intent that replaces it are one row.
func (r Reservation) Key() (string, error) {
	return IntentKey(r.InvocationID, IntentKindPublication)
}

// Same reports whether two reservations are the same claim by the same owner.
func (r Reservation) Same(other Reservation) bool {
	return r == other
}

// candidateReservationClaim is the claim a candidate presents when it commits
// its intent. A candidate naming no run reserved nothing and presents nothing;
// the reserved key then refuses it, which is the behaviour a foreign publisher
// must get.
func candidateReservationClaim(c Candidate) (*Reservation, error) {
	if c.RunID == "" {
		return nil, nil
	}
	claim, err := NewReservation(c.InvocationID, c.RunID)
	if err != nil {
		return nil, err
	}
	return &claim, nil
}

// ReservationReader is the read half of the reservation gates: the store's
// outbox lookup, satisfied by *store.ReadTx and everything embedding it.
type ReservationReader interface {
	GetOutbox(ctx context.Context, key string) (store.QueueEntry, error)
}

// ReservationTx additionally commits the reservation, so the claim rides the
// same transaction as the decision it belongs to. *store.InternalTx and
// *store.WriteTx both satisfy it.
type ReservationTx interface {
	ReservationReader
	EnqueueOutbox(ctx context.Context, key, kind string, payload []byte) (store.QueueEntry, bool, error)
}

// invocationState classifies what the reservation's key currently holds. Only
// states the caller may legitimately act on are represented; anything else
// (a foreign owner, a foreign kind, a quarantined row, an undecodable payload)
// is an error, not a state, so no gate can fall through to "available".
type invocationState string

const (
	// invocationFree: nothing holds the key.
	invocationFree invocationState = "free"
	// invocationReserved: our own reservation holds the key.
	invocationReserved invocationState = "reserved"
	// invocationCommitted: the publication intent that replaced our
	// reservation holds the key.
	invocationCommitted invocationState = "committed"
)

// allInvocationStates is the single registration point for the classifier's
// vocabulary; the zero value "" is invalid by design.
var allInvocationStates = []invocationState{
	invocationFree, invocationReserved, invocationCommitted,
}

func (s invocationState) valid() bool {
	switch s {
	case invocationFree, invocationReserved, invocationCommitted:
		return true
	default:
		return false
	}
}

// unhandledInvocationState builds the error a gate's trailing fallback returns.
// The predicate separates the two ways a gate can reach that fallback: a
// registered state some gate forgot to handle, which is a bug in the gate, and
// the invalid zero value, which is a state that was never classified at all.
func unhandledInvocationState(s invocationState) error {
	if s.valid() {
		return fmt.Errorf("publication invocation state %q is unhandled", s)
	}
	return fmt.Errorf(
		"publication invocation state %q is not one of %v", s, allInvocationStates)
}

// classifyInvocation reads the reservation's key and reports what holds it.
// Every row it returns a state for has been re-validated against the claim: a
// decoded reservation must be the same claim, and a decoded intent must name
// the same invocation as the key it was read under.
func classifyInvocation(
	ctx context.Context,
	reader ReservationReader,
	claim Reservation,
) (invocationState, store.QueueEntry, error) {
	if err := claim.Validate(); err != nil {
		return "", store.QueueEntry{}, err
	}
	key, err := claim.Key()
	if err != nil {
		return "", store.QueueEntry{}, err
	}
	entry, err := reader.GetOutbox(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return invocationFree, store.QueueEntry{}, nil
	}
	if err != nil {
		return "", store.QueueEntry{}, fmt.Errorf("read publication invocation %q: %w", key, err)
	}
	// The payload is opaque to the store, so the returned row is a
	// reconstruction boundary: check the row against its own key before
	// interpreting it (the store trust-boundary convention).
	if entry.IdempotencyKey != key {
		return "", store.QueueEntry{}, fmt.Errorf(
			"publication invocation %q read back key %q: %w",
			key, entry.IdempotencyKey, domain.ErrParentKeyMismatch,
		)
	}
	if entry.Quarantined() {
		return "", store.QueueEntry{}, fmt.Errorf(
			"publication invocation %q is quarantined: %w", key, ErrUnauthorizedPublication,
		)
	}
	switch entry.Kind {
	case IntentKindReservation:
		held, err := DecodeReservation(entry.Payload)
		if err != nil {
			// A row this build cannot read is not the reservation this key
			// should hold, so it fails closed under the same mismatch class as
			// a row naming the wrong parent: the caller must not proceed, and
			// must not read the invocation as free.
			return "", store.QueueEntry{}, fmt.Errorf(
				"publication invocation %q: %w: %w", key, err, domain.ErrParentKeyMismatch,
			)
		}
		if !held.Same(claim) {
			return "", store.QueueEntry{}, fmt.Errorf(
				"publication invocation %q is reserved by run %q: %w",
				key, held.RunID, ErrInvocationReserved,
			)
		}
		return invocationReserved, entry, nil
	case IntentKindPublication:
		// The intent carries no run id, so ownership here rests on the
		// reservation having held this key: a committed intent at a key this
		// claim reserved is this claim's own promoted intent. What is checked
		// is that the payload names the invocation its key does, so a foreign
		// payload filed under this key cannot pass as ours.
		intent, err := DecodeIntent(entry.Payload)
		if err != nil {
			return "", store.QueueEntry{}, fmt.Errorf(
				"publication invocation %q: %w: %w", key, err, domain.ErrParentKeyMismatch,
			)
		}
		if intent.InvocationID != claim.InvocationID {
			return "", store.QueueEntry{}, fmt.Errorf(
				"publication intent %q names invocation %q: %w",
				key, intent.InvocationID, domain.ErrParentKeyMismatch,
			)
		}
		return invocationCommitted, entry, nil
	}
	return "", store.QueueEntry{}, fmt.Errorf(
		"publication invocation %q holds kind %q: %w",
		key, entry.Kind, domain.ErrParentKeyMismatch,
	)
}

// CheckInvocationAvailable is the admission gate: it reports whether a workflow
// may still commit itself to publishing under this invocation ID. An already
// committed publication intent refuses, because the workflow would be binding
// itself to an identity somebody else already published under; the claim's own
// reservation passes, so re-admitting the same request converges.
//
// It only reads, so a caller can run it before deciding to commit anything.
func CheckInvocationAvailable(ctx context.Context, reader ReservationReader, claim Reservation) error {
	state, _, err := classifyInvocation(ctx, reader, claim)
	if err != nil {
		return err
	}
	switch state {
	case invocationFree, invocationReserved:
		return nil
	case invocationCommitted:
		key, keyErr := claim.Key()
		if keyErr != nil {
			return keyErr
		}
		return fmt.Errorf(
			"publication invocation %q already has a publisher intent: %w",
			key, domain.ErrParentKeyMismatch,
		)
	}
	return unhandledInvocationState(state)
}

// ClaimInvocation commits the reservation inside the caller's transaction, so
// the claim lands atomically with the decision that needs it. It converges
// rather than insisting on a fresh insert: re-running the same request finds
// its own reservation, and a replay after the workflow already published finds
// its own promoted intent. A different owner's reservation, a foreign kind, or
// an intent naming another invocation all fail closed.
func ClaimInvocation(ctx context.Context, tx ReservationTx, claim Reservation) error {
	state, _, err := classifyInvocation(ctx, tx, claim)
	if err != nil {
		return err
	}
	switch state {
	case invocationReserved, invocationCommitted:
		return nil
	case invocationFree:
		return insertReservation(ctx, tx, claim)
	}
	return unhandledInvocationState(state)
}

func insertReservation(ctx context.Context, tx ReservationTx, claim Reservation) error {
	key, err := claim.Key()
	if err != nil {
		return err
	}
	payload, err := claim.Encode()
	if err != nil {
		return err
	}
	entry, _, err := tx.EnqueueOutbox(ctx, key, IntentKindReservation, payload)
	if err != nil {
		return fmt.Errorf("reserve publication invocation %q: %w", key, err)
	}
	// EnqueueOutbox converges on whatever already holds the key, so the insert
	// proves nothing on its own: re-check the row it actually returned.
	if entry.IdempotencyKey != key || entry.Kind != IntentKindReservation ||
		!bytes.Equal(entry.Payload, payload) {
		return fmt.Errorf(
			"publication invocation %q holds kind %q after reserving: %w",
			key, entry.Kind, ErrInvocationReserved,
		)
	}
	return nil
}
