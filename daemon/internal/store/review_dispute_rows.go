package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// displayedDisputeIDs is canonical set identity, not display order. Item
// validation already rejects duplicate and unbound rows.
func displayedDisputeIDs(item domain.AttentionItem) string {
	if item.ReviewDispute == nil || len(item.ReviewDispute.Findings) == 0 {
		return ""
	}
	ids := make([]domain.FindingID, 0, len(item.ReviewDispute.Findings))
	for _, row := range item.ReviewDispute.Findings {
		ids = append(ids, row.FindingID)
	}
	slices.Sort(ids)
	body, _ := json.Marshal(ids) // A slice of string IDs always encodes.
	return string(body)
}

// gateReviewDisputeRows also checks non-routed bodies: removing all rows or
// the entire binding must not evade a previously recorded displayed set.
func (tx *ReadTx) gateReviewDisputeRows(ctx context.Context, item domain.AttentionItem) error {
	var stored string
	err := tx.tx.QueryRowContext(ctx,
		`SELECT finding_ids FROM review_dispute_rows WHERE item_id = ?`, item.ID).Scan(&stored)
	want := displayedDisputeIDs(item)
	if errors.Is(err, sql.ErrNoRows) {
		if want == "" {
			return nil
		}
		return errRowInconsistent
	}
	if err != nil {
		return err
	}
	if want == "" || stored != want {
		return errRowInconsistent
	}
	return nil
}

func (tx *WriteTx) validateReviewDisputeRowsUpdate(ctx context.Context, old, next domain.AttentionItem) error {
	if err := tx.gateReviewDisputeRows(ctx, old); err != nil {
		return err
	}
	if ids := displayedDisputeIDs(old); ids != "" && ids != displayedDisputeIDs(next) {
		return ErrImmutableConflict
	}
	return nil
}

func (tx *WriteTx) persistReviewDisputeRows(ctx context.Context, item domain.AttentionItem) error {
	ids := displayedDisputeIDs(item)
	if ids == "" {
		return nil
	}
	// Existing records were authenticated before any replay/transition return.
	// This insert also handles the first non-routed-to-routed transition.
	_, err := tx.tx.ExecContext(ctx,
		`INSERT INTO review_dispute_rows (item_id, finding_ids) VALUES (?, ?)
		 ON CONFLICT (item_id) DO NOTHING`, item.ID, ids)
	return err
}
