package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PutAttentionItemView records the first opening of an item's decision
// surface. The row is insert-only: when a view for the same (item, digest)
// already exists the call keeps it and its time, so the earliest committed
// open wins. It lives on WriteTx because the inbox on every device reads it
// through sync, so its write must ride the revision bump; the row carries that
// revision as its as_of_revision.
func (tx *WriteTx) PutAttentionItemView(ctx context.Context, view domain.AttentionItemView) error {
	if err := view.Validate(); err != nil {
		return fmt.Errorf("put attention item view: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO attention_item_views (item_id, decision_surface_digest, first_opened_at, as_of_revision)
VALUES (?, ?, ?, ?)
ON CONFLICT (item_id, decision_surface_digest) DO NOTHING`,
		view.ItemID, view.DecisionSurfaceDigest, formatTime(view.FirstOpenedAt), tx.asOfRevision); err != nil {
		return fmt.Errorf("put attention item view %q/%q: %w", view.ItemID, view.DecisionSurfaceDigest, err)
	}
	return nil
}

// GetAttentionItemView returns the view recorded for an item's decision
// surface and the revision that wrote it, or ErrNotFound when no device has
// opened that surface. A row that fails Validate is returned as an error; the
// caller treats the view as a display hint and decides how to degrade.
func (tx *ReadTx) GetAttentionItemView(
	ctx context.Context, itemID domain.ItemID, digest domain.Digest,
) (domain.AttentionItemView, int64, error) {
	var (
		firstOpenedAt string
		asOfRevision  int64
	)
	if err := tx.tx.QueryRowContext(ctx, `
SELECT first_opened_at, as_of_revision FROM attention_item_views
WHERE item_id = ? AND decision_surface_digest = ?`,
		itemID, digest).Scan(&firstOpenedAt, &asOfRevision); err != nil {
		return domain.AttentionItemView{}, 0, fmt.Errorf("get attention item view %q/%q: %w", itemID, digest, notFoundOr(err))
	}
	opened, err := parseTime(firstOpenedAt)
	if err != nil {
		return domain.AttentionItemView{}, 0, fmt.Errorf("get attention item view %q/%q first_opened_at: %w", itemID, digest, err)
	}
	view := domain.AttentionItemView{ItemID: itemID, DecisionSurfaceDigest: digest, FirstOpenedAt: opened}
	if err := view.Validate(); err != nil {
		return domain.AttentionItemView{}, 0, fmt.Errorf("get attention item view: %w", err)
	}
	return view, asOfRevision, nil
}
