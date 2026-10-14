package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const reviewDisputeRowsMigration = "0098_review_dispute_rows.sql"

// backfillReviewDisputeRows only authenticates legacy complete lists. A partial
// legacy list was invalid before this migration, so it cannot become authority
// merely because the new domain contract permits subsets.
func backfillReviewDisputeRows(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, body FROM attention_items WHERE item_type = 'review_dispute' ORDER BY id`)
	if err != nil {
		return err
	}
	type candidate struct {
		id   domain.ItemID
		body []byte
	}
	var candidates []candidate
	for rows.Next() {
		var row candidate
		if err := rows.Scan(&row.id, &row.body); err != nil {
			_ = rows.Close()
			return err
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, row := range candidates {
		item, err := decode[domain.AttentionItem](row.body)
		if err != nil {
			return fmt.Errorf("legacy review dispute %q: %w", row.id, err)
		}
		if item.ID != row.id || item.Type != domain.AttentionReviewDispute {
			return errRowInconsistent
		}
		ids := displayedDisputeIDs(item)
		if ids == "" {
			continue
		}
		if len(item.ReviewDispute.Findings) != len(item.ReviewDispute.FindingIDs) {
			return fmt.Errorf("legacy review dispute %q incomplete rows: %w", row.id, errRowInconsistent)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO review_dispute_rows (item_id, finding_ids) VALUES (?, ?)`, row.id, ids); err != nil {
			return err
		}
	}
	return nil
}
