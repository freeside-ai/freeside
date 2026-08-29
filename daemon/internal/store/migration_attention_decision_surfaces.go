package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const attentionDecisionSurfacesMigration = "0058_attention_decision_surfaces.sql"

// backfillAttentionDecisionSurfaces gives every existing attention item its
// epoch-1 decision surface (plan §4). The identity is derived from the item's
// structural fields and presentation slots alone. The body is decoded exactly
// as the read path decodes it, so derivability equals readability: a row that
// cannot yield a record (an undecodable or invalid body, a body naming another
// item) already fails every gated read, and the missing record keeps it
// refused there. The migration therefore changes no item's readability and
// cannot brick a daemon over a row it could never serve. It still fails on a
// store error.
func backfillAttentionDecisionSurfaces(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, body FROM attention_items ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list attention items: %w", err)
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
			return fmt.Errorf("read attention item: %w", err)
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read attention items: %w", err)
	}
	for _, row := range candidates {
		item, err := decode[domain.AttentionItem](row.body)
		if err != nil || item.ID != row.id {
			continue
		}
		surface, err := domain.NewDecisionSurface(item)
		if err != nil {
			continue
		}
		body, err := encode(surface)
		if err != nil {
			return fmt.Errorf("encode decision surface for attention item %s: %w", row.id, err)
		}
		if _, err := tx.ExecContext(ctx, insertDecisionSurfaceSQL,
			surface.ItemID, surface.Epoch, surface.Digest, body); err != nil {
			return fmt.Errorf("insert decision surface for attention item %s: %w", row.id, err)
		}
	}
	return nil
}
