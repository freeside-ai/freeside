package domain

import (
	"fmt"
	"time"
)

// AttentionItemView records that an item's decision surface, named by its
// digest, was first opened on some paired device at FirstOpenedAt: the
// daemon-stamped received_at of the earliest card_opened comprehension event
// bound to that digest (issue #2930). It is a Tier C display hint the inbox
// reads to mark unviewed items, never an input to any gate. The record is
// insert-only per (item, digest), so a revised decision surface, which carries
// a new digest, has no view until its card is opened again.
type AttentionItemView struct {
	ItemID                ItemID
	DecisionSurfaceDigest Digest
	FirstOpenedAt         time.Time
}

// Validate checks the record's shape: an item id, a content-address digest,
// and a non-zero UTC instant.
func (v AttentionItemView) Validate() error {
	if v.ItemID == "" {
		return fmt.Errorf("attention item view item_id: %w", ErrEmptyID)
	}
	if !isSHA256Digest(string(v.DecisionSurfaceDigest)) {
		return fmt.Errorf("attention item view %s decision_surface_digest %q: %w",
			v.ItemID, v.DecisionSurfaceDigest, ErrInvalidDigest)
	}
	return requireUTC(v.FirstOpenedAt, fmt.Sprintf("attention item view %s first_opened_at", v.ItemID))
}
