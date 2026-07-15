package signet

import (
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The signet service owns how a rejection surfaces at the attention-service
// boundary: both carriers below are what the API's 409 StaleVersionRejection
// renders from (api/openapi.yaml; the HTTP projection lands with #66). Store
// rejections that need no service-level shape (store.ErrNotFound,
// store.ErrActionNotOffered, store.ErrImmutableConflict) pass through
// wrapped and keep matching their store sentinels.

// ErrUnsupportedAction is returned for a genuinely new command (idempotency
// by command_id is judged first) whose action's accepted effect this boundary
// cannot represent yet:
// its transaction belongs to a later unit (discuss's conversation, snooze's
// timing update, start_with_changes's revised artifact and supersede), or the
// decision carries parameters or content DecisionPayload has no field for.
// Recording such a command would silently drop the user's data and, for
// discuss, let two devices commit against one item version where §5.14
// test 7 requires a single winner; failing loudly keeps the durable record
// faithful until the owning unit lifts the rejection (the pending group in
// actionOutcome).
var ErrUnsupportedAction = errors.New("action's transaction is not yet available at this boundary")

// ErrStaleVersion is returned when a genuinely new command was prepared
// against state that is no longer canonical: its ExpectedEntityVersion does
// not match the item's stored entity_version, or its payload bindings no
// longer describe the live item (§5.14 test 2). It is carried by a
// *StaleVersionError holding the replacement item; match the class with
// errors.Is and extract the replacement with errors.As.
var ErrStaleVersion = errors.New("command was prepared against superseded state")

// StaleVersionError reports a stale submission and carries the current item
// as the replacement the client re-renders and re-decides against, so no
// refetch is needed (§5.14 test 2). Snapshot is the replacement's persisted
// sync metadata: the API's 409 renders an AttentionItemSnapshot, so the
// entity_version the client resubmits against travels with the rejection
// instead of forcing the HTTP projection (#66) into a second, race-prone
// read. No side effect was applied.
type StaleVersionError struct {
	CommandID   string
	Replacement domain.AttentionItem
	Snapshot    store.Snapshot
}

func (e *StaleVersionError) Error() string {
	return fmt.Sprintf("command %q is stale: item %q is at version %d",
		e.CommandID, e.Replacement.ID, e.Replacement.ItemVersion)
}

// Is lets errors.Is(err, ErrStaleVersion) match the class while errors.As
// recovers the replacement item.
func (e *StaleVersionError) Is(target error) bool { return target == ErrStaleVersion }

// ErrClosedItem is returned when a genuinely new command targets an item
// whose lifecycle has concluded (issue #55): unlike ErrStaleVersion, no
// rebind-and-retry can ever succeed. It is carried by a *ClosedItemError
// holding the canonical item; match the class with errors.Is and extract the
// item with errors.As.
var ErrClosedItem = errors.New("item is no longer open for decisions")

// ClosedItemError reports a new command against a non-open item and carries
// the canonical item (the concluded outcome) the client should render, with
// its persisted sync metadata for the same 409 snapshot rendering
// StaleVersionError documents.
type ClosedItemError struct {
	CommandID string
	Item      domain.AttentionItem
	Snapshot  store.Snapshot
}

func (e *ClosedItemError) Error() string {
	return fmt.Sprintf("command %q rejected: item %q is %s at version %d",
		e.CommandID, e.Item.ID, e.Item.Status, e.Item.ItemVersion)
}

// Is lets errors.Is(err, ErrClosedItem) match the class while errors.As
// recovers the canonical item.
func (e *ClosedItemError) Is(target error) bool { return target == ErrClosedItem }

// translateRejection maps the store's acceptance rejections into the
// service's own carriers, so callers of the attention service never depend on
// store error types for the two shapes the API renders. The store carriers
// hold only the item, so the caller supplies the snapshot it read in the
// same transaction (single-connection SQLite: the store's re-read inside
// PutCommand cannot differ from it). Anything else passes through unchanged.
func translateRejection(err error, snap store.Snapshot) error {
	var stale *store.StaleCommandError
	if errors.As(err, &stale) {
		return &StaleVersionError{CommandID: stale.CommandID, Replacement: stale.Replacement, Snapshot: snap}
	}
	var closed *store.ClosedItemError
	if errors.As(err, &closed) {
		return &ClosedItemError{CommandID: closed.CommandID, Item: closed.Item, Snapshot: snap}
	}
	return err
}
