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

// putDecisionSurface maintains the item's decision-surface identity (plan §4)
// alongside the item row PutAttentionItem just wrote. A new item opens epoch
// 1; an existing item keeps its stored record unless its structural fields or
// presented set changed, in which case the epoch advances by exactly one
// under ValidateDecisionSurfaceTransition. Every other change to the item
// leaves the row untouched, and the byte-identical and canonical-equal replay
// branches never reach here, so telemetry never rewrites it. A transition
// against an existing item with no record is refused rather than repaired:
// the record's absence is row corruption every gated read already refuses,
// and a replay converging without a write leaves that refusal in place.
//
// old is the decoded pre-update item, nil only when the item is being
// created. The read-modify-write re-gates the stored record against it before
// deriving the next epoch: DecisionSurface proves the row is self-consistent,
// never that it describes the item it is stored against, and PutAttentionItem
// reaches here from a raw body without any gated reconstruction. Without that
// check a self-consistent row planted on some other surface would be laundered
// into a valid-looking successor, fabricating the epoch lineage a source
// record commits to and re-opening every read the re-gate had failed closed.
func (tx *WriteTx) putDecisionSurface(
	ctx context.Context, item domain.AttentionItem, old *domain.AttentionItem,
) error {
	if old == nil {
		surface, err := domain.NewDecisionSurface(item)
		if err != nil {
			return err
		}
		body, err := encode(surface)
		if err != nil {
			return err
		}
		_, err = tx.tx.ExecContext(ctx, insertDecisionSurfaceSQL,
			surface.ItemID, surface.Epoch, surface.Digest, body)
		return err
	}
	current, err := tx.DecisionSurface(ctx, item.ID)
	if errors.Is(err, ErrNotFound) {
		return errRowInconsistent
	}
	if err != nil {
		return err
	}
	if !current.Matches(*old) {
		return errRowInconsistent
	}
	next, advanced, err := domain.NextDecisionSurface(current, item)
	if err != nil {
		return err
	}
	if !advanced {
		return nil
	}
	if err := domain.ValidateDecisionSurfaceTransition(current, next); err != nil {
		return mapTransition(err)
	}
	body, err := encode(next)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, advanceDecisionSurfaceSQL,
		next.Epoch, next.Digest, body, next.ItemID)
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
	if !surface.Matches(item) {
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
