package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Transition validators answer a different question than the field-level
// Validate methods: not "is this value well-formed" but "is this a legal
// successor to what is already stored". They enforce the immutability and
// forward-only rules a persisted aggregate obeys between its stored version and
// an update (plan §5.3, §5.14, §4), so any writer (the store today, the engine
// or importer later) reuses one definition instead of re-deriving it.
//
// A transition is between two versions of the same aggregate: each validator
// first rejects a change of identity (the aggregate's key), so a caller that
// does not fetch old by key cannot pass a different aggregate as a successor.
//
// Each failure wraps one of two classes, so a caller maps it onto its own
// boundary errors without string matching:
//   - ErrImmutableTransition: identity, another fixed field, recorded
//     history, or a terminal lifecycle outcome would change.
//   - ErrStaleTransition: an update fails to advance a version or lifecycle.
//
// A byte-identical replay (a retried write) is the caller's concern, not these
// validators': callers that converge identical writes must short-circuit before
// calling a validator, since an unchanged update does not advance a version and
// these would reject it.

// ValidateRunTransition reports whether updated is a legal successor to the
// stored run old. Project, approved spec, and resolved policy are fixed at run
// creation (plan §5.3 binds a run to its spec and policy digests), and
// stages/attempts are recorded history: an update may only append.
func ValidateRunTransition(old, updated Run) error {
	if updated.ID != old.ID {
		return fmt.Errorf("run %s: identity would change from %s: %w", updated.ID, old.ID, ErrImmutableTransition)
	}
	if updated.ProjectID != old.ProjectID || updated.SpecDigest != old.SpecDigest ||
		updated.PolicyDigest != old.PolicyDigest || !stagesExtend(old.Stages, updated.Stages) {
		return fmt.Errorf("run %s: fixed bindings or recorded history would change: %w", updated.ID, ErrImmutableTransition)
	}
	return nil
}

// ValidateConversationTransition reports whether updated is a legal successor to
// the stored conversation old. Messages are immutable and corrections are new
// messages (plan §5.14): an update must carry every stored message unchanged and
// may only append.
func ValidateConversationTransition(old, updated Conversation) error {
	if updated.ID != old.ID {
		return fmt.Errorf("conversation %s: identity would change from %s: %w", updated.ID, old.ID, ErrImmutableTransition)
	}
	if len(updated.Messages) < len(old.Messages) {
		return fmt.Errorf("conversation %s: stored messages would be dropped: %w", updated.ID, ErrImmutableTransition)
	}
	same, err := jsonEqual(old.Messages, updated.Messages[:len(old.Messages)])
	if err != nil {
		return fmt.Errorf("conversation %s: %w", updated.ID, err)
	}
	if !same {
		return fmt.Errorf("conversation %s: stored messages would be rewritten: %w", updated.ID, ErrImmutableTransition)
	}
	return nil
}

// itemStatusSuccessors returns the statuses a version-advancing update may
// move status to. A same-status update is always legal; an unlisted pair is
// not. The terminal statuses (resolved, superseded, dismissed, expired) admit
// no successors: an item's recorded final outcome never reopens, a fresh
// decision is a new item (plan §4 lifecycle). A switch, not a map, so the
// exhaustive linter forces a future status to declare its successors instead
// of silently defaulting to terminal.
func itemStatusSuccessors(status ItemStatus) []ItemStatus {
	switch status {
	case StatusOpen:
		return []ItemStatus{StatusResolved, StatusSuperseded, StatusDismissed, StatusExpired}
	case StatusResolved, StatusSuperseded, StatusDismissed, StatusExpired:
		return nil
	}
	return nil
}

// ValidateAttentionItemTransition reports whether updated is a legal successor to
// the stored item old. What an item is about is fixed at creation: transitions
// bump item_version and evolve status/evidence on the same identity, and a
// different subject or type is a new (superseding) item, never a retarget (plan
// §4, §5.14). A changed body must move the version forward, or a stale copy could
// roll back a later transition (a resolved v2 overwritten by an open v1). Status
// moves follow itemStatusSuccessors: a terminal status is final.
func ValidateAttentionItemTransition(old, updated AttentionItem) error {
	if updated.ID != old.ID {
		return fmt.Errorf("attention item %s: identity would change from %s: %w", updated.ID, old.ID, ErrImmutableTransition)
	}
	sameSubject, err := jsonEqual(old.Subject, updated.Subject)
	if err != nil {
		return fmt.Errorf("attention item %s: %w", updated.ID, err)
	}
	if updated.ProjectID != old.ProjectID || updated.Type != old.Type || !sameSubject {
		return fmt.Errorf("attention item %s: fixed bindings would change: %w", updated.ID, ErrImmutableTransition)
	}
	if updated.ItemVersion <= old.ItemVersion {
		return fmt.Errorf("attention item %s: item_version %d does not advance stored %d: %w",
			updated.ID, updated.ItemVersion, old.ItemVersion, ErrStaleTransition)
	}
	if updated.Status != old.Status && !slices.Contains(itemStatusSuccessors(old.Status), updated.Status) {
		return fmt.Errorf("attention item %s: status %q is terminal and cannot become %q: %w",
			updated.ID, old.Status, updated.Status, ErrImmutableTransition)
	}
	return nil
}

// ValidateAttentionDeliveryTransition reports whether updated is a legal
// successor to the stored delivery old. The lifecycle only moves forward: a
// stale retry must not roll an opened delivery back to submitted and drop the
// receipts timing aggregates depend on; and an advance preserves the receipts
// already recorded (plan §4).
func ValidateAttentionDeliveryTransition(old, updated AttentionDelivery) error {
	// A delivery's identity is its (item, device, channel, attempt) key; a
	// change to any of them is a different delivery, not a successor.
	if updated.ItemID != old.ItemID || updated.DeviceID != old.DeviceID ||
		updated.Channel != old.Channel || updated.Attempt != old.Attempt {
		return fmt.Errorf("delivery %s/%s/%s/%d: identity would change from %s/%s/%s/%d: %w",
			updated.ItemID, updated.DeviceID, updated.Channel, updated.Attempt,
			old.ItemID, old.DeviceID, old.Channel, old.Attempt, ErrImmutableTransition)
	}
	if deliveryRank(updated.Status) <= deliveryRank(old.Status) {
		return fmt.Errorf("delivery %s/%s/%s/%d: delivery_status %q does not advance stored %q: %w",
			updated.ItemID, updated.DeviceID, updated.Channel, updated.Attempt, updated.Status, old.Status, ErrStaleTransition)
	}
	if !updated.SubmittedAt.Equal(old.SubmittedAt) ||
		(old.ChannelAcceptedAt != nil && !timesEqual(updated.ChannelAcceptedAt, old.ChannelAcceptedAt)) ||
		(old.OpenedAt != nil && !timesEqual(updated.OpenedAt, old.OpenedAt)) {
		return fmt.Errorf("delivery %s/%s/%s/%d: recorded receipts would change: %w",
			updated.ItemID, updated.DeviceID, updated.Channel, updated.Attempt, ErrImmutableTransition)
	}
	return nil
}

// stagesExtend reports whether updated preserves old's recorded execution
// history: every existing stage keeps its identity and name, every existing
// attempt is unchanged, and growth is append-only.
func stagesExtend(old, updated []Stage) bool {
	if len(updated) < len(old) {
		return false
	}
	for i, os := range old {
		ns := updated[i]
		if ns.ID != os.ID || ns.RunID != os.RunID || ns.Name != os.Name {
			return false
		}
		if len(ns.Attempts) < len(os.Attempts) {
			return false
		}
		for j, oa := range os.Attempts {
			if ns.Attempts[j] != oa {
				return false
			}
		}
	}
	return true
}

// jsonEqual compares two values by their canonical JSON, the byte form the store
// persists.
func jsonEqual(a, b any) (bool, error) {
	ab, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(ab) == string(bb), nil
}

// deliveryRank orders the delivery lifecycle so a transition can require it to
// strictly advance. An unknown status ranks 0, below every real status.
func deliveryRank(status DeliveryStatus) int {
	switch status {
	case DeliverySubmitted:
		return 1
	case DeliveryChannelAccepted:
		return 2
	case DeliveryOpened:
		return 3
	}
	return 0
}

// timesEqual compares an optional receipt pair, nil meaning not yet recorded.
func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
