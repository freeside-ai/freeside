package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const attentionDecisionSurfacesMigration = "0058_attention_decision_surfaces.sql"

const retiredAdjudicateAction domain.Action = "adjudicate"

// backfillAttentionDecisionSurfaces gives every existing attention item its
// epoch-1 decision surface (plan §4). The identity is derived from the item's
// structural fields and presentation slots alone. Before decoding, the
// migration removes the retired decorative adjudicate action from legacy item
// bodies so records that were valid at schema 0057 remain readable. A row that
// still cannot yield a record (an undecodable or invalid body, a body naming
// another item) already failed every gated read, and the missing record keeps
// it refused there. Store errors still fail the migration.
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
		itemBody, retired, err := retireLegacyAdjudicate(row.body)
		if err != nil {
			continue
		}
		item, err := decode[domain.AttentionItem](itemBody)
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
		if retired {
			if _, err := tx.ExecContext(ctx,
				`UPDATE attention_items SET body = ? WHERE id = ?`, string(itemBody), row.id,
			); err != nil {
				return fmt.Errorf("retire legacy adjudicate action for attention item %s: %w", row.id, err)
			}
		}
		if _, err := tx.ExecContext(ctx, insertDecisionSurfaceSQL,
			surface.ItemID, surface.Epoch, surface.Digest, body); err != nil {
			return fmt.Errorf("insert decision surface for attention item %s: %w", row.id, err)
		}
	}
	return nil
}

// retireLegacyAdjudicate removes every occurrence of the old decorative
// action while preserving the rest of the item body for current validation.
// Invalid legacy JSON stays untouched and follows the migration's existing
// unreadable-row tolerance.
func retireLegacyAdjudicate(body []byte) ([]byte, bool, error) {
	var object map[string]json.RawMessage
	if err := decodeMigrationJSON(body, &object); err != nil {
		return nil, false, err
	}
	rawActions, ok := object["requested_decision"]
	if !ok {
		return body, false, nil
	}
	var actions []domain.Action
	if err := decodeMigrationJSON(rawActions, &actions); err != nil {
		return nil, false, err
	}
	retained := make([]domain.Action, 0, len(actions))
	for _, action := range actions {
		if action != retiredAdjudicateAction {
			retained = append(retained, action)
		}
	}
	if len(retained) == len(actions) {
		return body, false, nil
	}
	encodedActions, err := json.Marshal(retained)
	if err != nil {
		return nil, false, err
	}
	object["requested_decision"] = encodedActions
	rewritten, err := json.Marshal(object)
	if err != nil {
		return nil, false, err
	}
	return rewritten, true, nil
}
