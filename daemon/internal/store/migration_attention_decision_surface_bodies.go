package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const attentionDecisionSurfaceBodiesMigration = "0059_attention_decision_surface_bodies.sql"

// backfillAttentionDecisionSurfaceBodies copies each authenticated surface
// epoch and digest into its item body. Rows that were already underivable stay
// untouched and unreadable, matching migration 0058's tolerance.
func backfillAttentionDecisionSurfaceBodies(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT i.id, i.body, s.epoch, s.digest, s.body
        FROM attention_items AS i
        LEFT JOIN attention_decision_surfaces AS s ON s.item_id = i.id
        ORDER BY i.id`)
	if err != nil {
		return fmt.Errorf("list attention decision surface bodies: %w", err)
	}
	type candidate struct {
		id          domain.ItemID
		itemBody    []byte
		epoch       sql.NullInt64
		surfaceHash sql.NullString
		surfaceBody []byte
	}
	var candidates []candidate
	for rows.Next() {
		var row candidate
		if err := rows.Scan(&row.id, &row.itemBody, &row.epoch, &row.surfaceHash, &row.surfaceBody); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read attention decision surface body: %w", err)
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read attention decision surface bodies: %w", err)
	}
	for _, row := range candidates {
		if !row.epoch.Valid || !row.surfaceHash.Valid || len(row.surfaceBody) == 0 {
			continue
		}
		item, err := decode[domain.AttentionItem](row.itemBody)
		if err != nil || item.ID != row.id {
			continue
		}
		surface, err := decode[domain.DecisionSurface](row.surfaceBody)
		if err != nil || surface.ItemID != row.id || surface.Epoch != int(row.epoch.Int64) ||
			surface.Digest != domain.Digest(row.surfaceHash.String) || !surface.Matches(item) {
			continue
		}
		item.DecisionSurface = domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}
		item.Recommendation = nil
		body, err := encode(item)
		if err != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, body, row.id); err != nil {
			return fmt.Errorf("rewrite attention item %s decision surface body: %w", row.id, err)
		}
	}
	return nil
}
