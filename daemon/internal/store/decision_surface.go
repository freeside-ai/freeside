package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const insertDecisionSurfaceSQL = `
INSERT INTO attention_decision_surfaces (item_id, epoch, digest, body)
VALUES (?, ?, ?, ?)`

const advanceDecisionSurfaceSQL = `
UPDATE attention_decision_surfaces
SET epoch = ?, digest = ?, body = ?
WHERE item_id = ?`

// prepareDecisionSurface derives the identity PutAttentionItem will copy into
// the item body. It performs the read-modify-write re-gate before any item row
// changes, including the item-side epoch-and-digest comparison that prevents a
// surfaces-table-only writer from choosing an epoch.
func (tx *WriteTx) prepareDecisionSurface(
	ctx context.Context, item domain.AttentionItem, old *domain.AttentionItem,
) (domain.DecisionSurface, bool, bool, error) {
	if old == nil {
		surface, err := domain.NewDecisionSurface(item)
		return surface, true, true, err
	}
	current, err := tx.DecisionSurface(ctx, item.ID)
	if errors.Is(err, ErrNotFound) {
		return domain.DecisionSurface{}, false, false, errRowInconsistent
	}
	if err != nil {
		return domain.DecisionSurface{}, false, false, err
	}
	if !current.Matches(*old) || old.DecisionSurface != (domain.DecisionSurfaceRef{
		Epoch: current.Epoch, Digest: current.Digest,
	}) {
		return domain.DecisionSurface{}, false, false, errRowInconsistent
	}
	next, advanced, err := domain.NextDecisionSurface(current, item)
	if err != nil {
		return domain.DecisionSurface{}, false, false, err
	}
	if !advanced {
		return current, false, false, nil
	}
	if err := domain.ValidateDecisionSurfaceTransition(current, next); err != nil {
		return domain.DecisionSurface{}, false, false, mapTransition(err)
	}
	return next, false, true, nil
}

// persistDecisionSurface writes the identity prepared before the item body was
// encoded. Creation runs after the item insert because of the foreign key;
// advancement updates the existing row in the same transaction.
func (tx *WriteTx) persistDecisionSurface(
	ctx context.Context, surface domain.DecisionSurface, created, changed bool,
) error {
	if !changed {
		return nil
	}
	body, err := encode(surface)
	if err != nil {
		return err
	}
	if created {
		_, err = tx.tx.ExecContext(ctx, insertDecisionSurfaceSQL,
			surface.ItemID, surface.Epoch, surface.Digest, body)
		return err
	}
	_, err = tx.tx.ExecContext(ctx, advanceDecisionSurfaceSQL,
		surface.Epoch, surface.Digest, body, surface.ItemID)
	return err
}

// gateDecisionSurface is the reconstruction re-gate: the persisted record is
// authority, so an item whose record is missing, fails its own digest
// recomputation, or disagrees with the item's structural fields or presented
// set fails closed as row-inconsistent (plan §4). It runs on every gated read
// so restart and raw-row tampering reject alike.
func (tx *ReadTx) gateDecisionSurface(ctx context.Context, item domain.AttentionItem) error {
	surface, err := tx.DecisionSurface(ctx, item.ID)
	if errors.Is(err, ErrNotFound) {
		return errRowInconsistent
	}
	if err != nil {
		return err
	}
	if !surface.Matches(item) || item.DecisionSurface != (domain.DecisionSurfaceRef{
		Epoch: surface.Epoch, Digest: surface.Digest,
	}) {
		return errRowInconsistent
	}
	return nil
}

// DecisionSurface returns the item's current decision-surface identity. The
// decoded body re-validates (its digest must recompute) and must agree with
// the row's key columns; a divergent row is refused. The recommendation
// source-record checks (#917) and producers pre-committing to a prospective
// epoch read the current record here.
//
// The row is authenticated against itself only, never against the item it is
// stored for: that comparison is gateDecisionSurface's, and this accessor is
// not a reconstruction. A caller authenticating a source record against the
// current surface must therefore hold an item obtained through a gated read
// (GetAttentionItem or GetAttentionItemRecord), or it authenticates against a
// record no gate has tied to an item.
func (tx *ReadTx) DecisionSurface(ctx context.Context, itemID domain.ItemID) (domain.DecisionSurface, error) {
	var (
		epoch  int
		digest string
		body   []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT epoch, digest, body FROM attention_decision_surfaces WHERE item_id = ?`, itemID,
	).Scan(&epoch, &digest, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DecisionSurface{}, fmt.Errorf("get decision surface %q: %w", itemID, ErrNotFound)
	}
	if err != nil {
		return domain.DecisionSurface{}, fmt.Errorf("get decision surface %q: %w", itemID, err)
	}
	surface, err := decode[domain.DecisionSurface](body)
	if err != nil {
		return domain.DecisionSurface{}, fmt.Errorf("get decision surface %q: %w", itemID, err)
	}
	if surface.ItemID != itemID || surface.Epoch != epoch || string(surface.Digest) != digest {
		return domain.DecisionSurface{}, fmt.Errorf("get decision surface %q: %w", itemID, errRowInconsistent)
	}
	return surface, nil
}
