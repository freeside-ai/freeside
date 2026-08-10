package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The persistence kernel supplies common mechanisms; each entity contract
// selects the ones it needs. Required lookups wrap missing rows with ErrNotFound
// and preserve other reconstruction failures through contextual wrapping;
// optional lookups report absence explicitly. Typed-domain writes validate
// proposed values, and aggregates enforce old-to-new transitions when their
// domains define them. Records routed through putImmutable accept only
// byte-identical replays and reject same-key rewrites. Typed-domain
// reconstruction validates decoded values and cross-checks extracted columns
// where present; authority-bearing reads re-run applicable current trust gates
// instead of trusting persisted eligibility. Opaque and historical records
// retain their owner-specific validation rules.

// ErrNotFound is returned (wrapped, with the entity and id) when a required
// lookup's row does not exist. Optional lookups report absence separately.
var ErrNotFound = errors.New("not found")

// errRowInconsistent marks a row whose JSON body disagrees with its extracted
// key columns: the store's foreign keys and lookups act on the columns, so a
// divergent body would be trusted domain data with unenforced keys.
// Reconstruction paths with both representations cross-check them and fail
// loudly instead of returning a divergent body.
var errRowInconsistent = errors.New("stored row body inconsistent with its key columns")

// ErrImmutableConflict is returned (wrapped, with the entity and id) when a
// write-once entity is re-put with different content under an existing key.
// The domain contract makes these values immutable (a correction is a new
// value with a new version or identity, never an in-place edit), so the store
// tolerates only byte-identical replays: a retry converges, a rewrite fails.
var ErrImmutableConflict = errors.New("immutable row already exists with different content")

// ErrStaleWrite is returned (wrapped, with the entity and id) when an update
// to a transition-guarded current-state aggregate does not move its state
// forward: an attention item whose item_version is not beyond the stored one,
// or a delivery whose lifecycle status regresses. Retries replaying the
// identical bytes converge silently; a genuinely stale body must fail rather
// than roll back state (§5.14 optimistic concurrency).
var ErrStaleWrite = errors.New("write is stale: stored state is newer")

// ErrStaleCommand is returned when a new command's pinned bindings no longer
// describe the live attention item: the item advanced (its version, PR head, or
// rendered digest set changed) after the command was prepared, so the
// submission is stale (§5.14 test 2). It is carried by a *StaleCommandError,
// which also holds the current item as the canonical replacement state; match
// the class with errors.Is and extract the replacement with errors.As.
var ErrStaleCommand = errors.New("command bindings no longer match the item")

// ErrActionNotOffered is returned when a command's action is a valid enum value
// but not one the live item offered in its requested_decision (plan §4: the
// offered set is item-specific, "approve" is not universal). Rejecting it keeps
// the durable record faithful to the choices rendered to the user: a client
// cannot record an action the item never presented.
var ErrActionNotOffered = errors.New("command action is not offered by the item")

// StaleCommandError reports a stale command submission and carries the current
// attention item as the replacement the caller must re-render and re-decide
// against (plan §4 lifecycle, §5.14 test 2). Idempotent replays are handled
// before this check, so a StaleCommandError only ever names a genuinely new
// command_id whose bound inputs drifted, never a retry of a committed one.
type StaleCommandError struct {
	CommandID   string
	Replacement domain.AttentionItem
}

func (e *StaleCommandError) Error() string {
	return fmt.Sprintf("command %q is stale: item %q is at version %d",
		e.CommandID, e.Replacement.ID, e.Replacement.ItemVersion)
}

// Is lets errors.Is(err, ErrStaleCommand) match the class while errors.As
// recovers the replacement item.
func (e *StaleCommandError) Is(target error) bool { return target == ErrStaleCommand }

// ErrClosedItem is returned when a genuinely new command targets an attention
// item whose status is no longer open (issue #55). Unlike ErrStaleCommand it
// does not depend on the command's bound version: the item's lifecycle has
// concluded, so no rebind-and-retry can ever succeed. It is carried by a
// *ClosedItemError; match the class with errors.Is and extract the canonical
// item with errors.As.
var ErrClosedItem = errors.New("item is no longer open for decisions")

// ClosedItemError reports a new command against a non-open item and carries
// the current attention item as the canonical state the caller should render
// (plan §4 lifecycle). Idempotent replays are handled before this check, so a
// ClosedItemError only ever names a genuinely new command_id, never a retry of
// a committed one (§5.14 test 4).
type ClosedItemError struct {
	CommandID string
	Item      domain.AttentionItem
}

func (e *ClosedItemError) Error() string {
	return fmt.Sprintf("command %q rejected: item %q is %s at version %d",
		e.CommandID, e.Item.ID, e.Item.Status, e.Item.ItemVersion)
}

// Is lets errors.Is(err, ErrClosedItem) match the class while errors.As
// recovers the canonical item.
func (e *ClosedItemError) Is(target error) bool { return target == ErrClosedItem }

// validator is implemented by every persisted domain type. Puts validate
// before writing and Gets validate after reading, so a corrupt row fails
// loudly at the boundary instead of leaking an invalid value into the daemon.
type validator interface{ Validate() error }

// mapTransition translates a domain transition-validator failure into the
// store's own boundary error. The domain validators own the transition rules
// (one definition reused by every writer of that aggregate); the store owns how
// a rejection surfaces at its edge. Double-wrapping keeps the store sentinel
// matchable by errors.Is while preserving the domain detail in the chain, so
// callers keep matching ErrImmutableConflict / ErrStaleWrite unchanged.
func mapTransition(err error) error {
	switch {
	case errors.Is(err, domain.ErrImmutableTransition):
		return fmt.Errorf("%w: %w", ErrImmutableConflict, err)
	case errors.Is(err, domain.ErrStaleTransition):
		return fmt.Errorf("%w: %w", ErrStaleWrite, err)
	default:
		return err
	}
}

// encode validates v and returns its canonical JSON body, as a string so it
// binds as TEXT (a []byte binds as BLOB, which a STRICT TEXT column rejects).
func encode(v validator) (string, error) {
	if err := v.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// storedRowCanonicalizer is implemented by a decoded entity that can carry a
// field written under an older, looser encoding than the current Validate
// accepts. decode rewrites those fields to their canonical spelling before
// Validate, so a legacy row converges instead of being refused; the rewrite
// must be lossless so put-idempotence's canonical re-encode still matches
// (issue #553).
type storedRowCanonicalizer interface{ CanonicalizeStoredRow() }

// decode unmarshals a stored body and re-validates it: Validate is the
// deserialization backstop for values that bypassed their constructor. A row
// whose type canonicalizes stored fields is rewritten first, so a legacy
// spelling reaches Validate in the form it now demands.
func decode[T validator](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	if c, ok := any(&v).(storedRowCanonicalizer); ok {
		c.CanonicalizeStoredRow()
	}
	if err := v.Validate(); err != nil {
		return v, fmt.Errorf("stored row invalid: %w", err)
	}
	return v, nil
}

// scanner is the shared surface of *sql.Row and *sql.Rows: it lets one
// reconstruction function (scan, decode, cross-check the extracted columns,
// range-check the store-stamped metadata, re-run the policy gate) serve both a
// single-entity Get and a collection List, so a gate added to one path cannot
// be missed on the other.
type scanner interface{ Scan(dest ...any) error }

// putImmutable inserts a write-once row (INSERT ... ON CONFLICT DO NOTHING),
// tolerating only a byte-identical replay of an existing key: canonical
// json.Marshal is deterministic, so a retried Put of the same value converges
// on the original row (no entity_version churn, nothing new for sync to
// observe), while a same-key write with different content fails with
// ErrImmutableConflict. On InternalTx so the non-synchronized write-once
// records (pairing codes) share it; the synchronized callers all hold a
// WriteTx, whose statements stamp its as_of_revision.
func (tx *InternalTx) putImmutable(ctx context.Context, insertSQL string, insertArgs []any, selectBodySQL string, keyArgs []any, body string) error {
	_, err := tx.putImmutableInserted(ctx, insertSQL, insertArgs, selectBodySQL, keyArgs, body)
	return err
}

// putImmutableInserted additionally reports whether this call inserted the
// row (false on a byte-identical replay), for callers whose side effects
// belong only to the first write — the observation milestones ride the
// inserting transaction and must not be minted again by a replay against a
// database that no longer has them (migration 0024's no-backfill rule).
func (tx *InternalTx) putImmutableInserted(ctx context.Context, insertSQL string, insertArgs []any, selectBodySQL string, keyArgs []any, body string) (bool, error) {
	res, err := tx.tx.ExecContext(ctx, insertSQL, insertArgs...)
	if err != nil {
		return false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted > 0 {
		return true, nil
	}
	var existing string
	if err := tx.tx.QueryRowContext(ctx, selectBodySQL, keyArgs...).Scan(&existing); err != nil {
		return false, err
	}
	if existing != body {
		return false, ErrImmutableConflict
	}
	return false, nil
}

// existingBody fetches the current body for an aggregate's key, or nil when
// the row does not exist. The query must be a fixed statement supplied by the
// caller. On InternalTx for the same reason as putImmutable.
func (tx *InternalTx) existingBody(ctx context.Context, selectSQL string, keyArgs ...any) ([]byte, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx, selectSQL, keyArgs...).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}

// Snapshot is the persisted §5.14 sync metadata read alongside a row: the
// per-row EntityVersion a ClientCommand's expected_entity_version is checked
// against, and the AsOfRevision of the transaction that last wrote the row.
// Both are stamped by the store's own Puts, never by callers, and are
// range-checked alongside the other extracted columns on those reconstruction
// paths.
type Snapshot struct {
	EntityVersion int64
	AsOfRevision  int64
}

// notFoundOr maps sql.ErrNoRows to ErrNotFound and passes every other error
// through.
func notFoundOr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
