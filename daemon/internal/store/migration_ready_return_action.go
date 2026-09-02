package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const readyReturnActionMigration = "0062_ready_return_action.sql"

var legacyProductionReadyActions = []domain.Action{
	domain.ActionOpenPR,
	domain.ActionMarkSeen,
	domain.ActionDismiss,
	domain.ActionStop,
}

var productionReadyActions = []domain.Action{
	domain.ActionOpenPR,
	domain.ActionReturnToAgent,
	domain.ActionMarkSeen,
	domain.ActionDismiss,
	domain.ActionStop,
}

type readyReturnRewrite struct {
	item            domain.AttentionItem
	surface         domain.DecisionSurface
	entityVersion   int64
	asOfRevision    int64
	oldItemBody     string
	oldSurfaceBody  string
	oldSurfaceEpoch int
	oldSurfaceHash  domain.Digest
}

// addReturnActionToProductionReadyItems advances only authenticated-looking
// production ready surfaces created by the immediately preceding producer
// contract. Fake ready items have no ReadyItemPRBinding and stay untouched.
// Any malformed, retargeted, or noncanonical row is skipped and remains
// fail-closed at the ordinary reconstruction boundary.
func addReturnActionToProductionReadyItems(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT i.id
		FROM attention_items AS i
		JOIN ready_item_pr_bindings AS b ON b.item_id = i.id
		WHERE i.item_type = 'ready_for_final_review' AND i.status = 'open'
		ORDER BY i.id`)
	if err != nil {
		return fmt.Errorf("list production ready items: %w", err)
	}
	var ids []domain.ItemID
	for rows.Next() {
		var id domain.ItemID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read production ready item id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read production ready item ids: %w", err)
	}

	rewrites := make([]readyReturnRewrite, 0, len(ids))
	for _, id := range ids {
		rewrite, ok, err := readyReturnCandidate(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("authenticate production ready item %s: %w", id, err)
		}
		if ok {
			rewrites = append(rewrites, rewrite)
		}
	}
	if len(rewrites) == 0 {
		return nil
	}

	var revision int64
	if err := tx.QueryRowContext(ctx, `UPDATE server_state
		SET revision = revision + 1 WHERE id = 1 RETURNING revision`).Scan(&revision); err != nil {
		return fmt.Errorf("advance ready-return migration revision: %w", err)
	}
	for _, rewrite := range rewrites {
		itemBody, err := encode(rewrite.item)
		if err != nil {
			return fmt.Errorf("encode production ready item %s: %w", rewrite.item.ID, err)
		}
		surfaceBody, err := encode(rewrite.surface)
		if err != nil {
			return fmt.Errorf("encode production ready surface %s: %w", rewrite.item.ID, err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE attention_decision_surfaces
			SET epoch = ?, digest = ?, body = ?
			WHERE item_id = ? AND epoch = ? AND digest = ? AND body = ?`,
			rewrite.surface.Epoch, rewrite.surface.Digest, surfaceBody,
			rewrite.item.ID, rewrite.oldSurfaceEpoch, rewrite.oldSurfaceHash,
			rewrite.oldSurfaceBody)
		if err != nil {
			return fmt.Errorf("advance production ready surface %s: %w", rewrite.item.ID, err)
		}
		if err := requireOneMigrationRow(result); err != nil {
			return fmt.Errorf("advance production ready surface %s: %w", rewrite.item.ID, err)
		}
		result, err = tx.ExecContext(ctx, `UPDATE attention_items
			SET entity_version = ?, as_of_revision = ?, body = ?
			WHERE id = ? AND entity_version = ? AND as_of_revision = ? AND body = ?`,
			rewrite.entityVersion+1, revision, itemBody, rewrite.item.ID,
			rewrite.entityVersion, rewrite.asOfRevision, rewrite.oldItemBody)
		if err != nil {
			return fmt.Errorf("rewrite production ready item %s: %w", rewrite.item.ID, err)
		}
		if err := requireOneMigrationRow(result); err != nil {
			return fmt.Errorf("rewrite production ready item %s: %w", rewrite.item.ID, err)
		}
	}
	return nil
}

func readyReturnCandidate(
	ctx context.Context,
	tx *sql.Tx,
	id domain.ItemID,
) (readyReturnRewrite, bool, error) {
	// This runs at schema version 0062, before 0063 added readiness_detail,
	// so the column is projected as NULL: every row here predates the detail,
	// and the shared scanner's column list must still line up.
	item, snapshot, err := scanAttentionItemRecord(tx.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture,
		        subject_run_id, readiness_summary, NULL AS readiness_detail, yield_history,
		        entity_version, as_of_revision, body
		 FROM attention_items WHERE id = ?`, id))
	if err != nil {
		return readyReturnRewrite{}, false, nil
	}
	if item.ID != id || item.Type != domain.AttentionReadyForFinalReview ||
		item.Status != domain.StatusOpen || item.Subject.Type != domain.SubjectRun ||
		item.Subject.RunID == nil || item.Subject.ID != domain.SubjectID(*item.Subject.RunID) ||
		item.ID != domain.ProductionReadyItemID(*item.Subject.RunID) ||
		!slices.Equal(item.RequestedDecision, legacyProductionReadyActions) {
		return readyReturnRewrite{}, false, nil
	}

	reader := ReadTx{tx: tx}
	surface, err := reader.DecisionSurface(ctx, id)
	if err != nil || !surface.Matches(item) ||
		item.DecisionSurface != (domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}) {
		return readyReturnRewrite{}, false, nil
	}
	anchored, err := reader.getAttentionItemPRReference(ctx, id)
	if err != nil || item.PRReference == nil || *item.PRReference != anchored {
		return readyReturnRewrite{}, false, nil
	}
	binding, ok, err := readyBindingForMigration(ctx, tx, item)
	if err != nil {
		return readyReturnRewrite{}, false, err
	}
	if !ok || binding.RunID != *item.Subject.RunID || binding.HeadSHA != item.PRHeadSHA ||
		binding.Repo != anchored.Repo || binding.PRNumber != anchored.Number {
		return readyReturnRewrite{}, false, nil
	}

	var oldItemBody, oldSurfaceBody string
	if err := tx.QueryRowContext(ctx, `SELECT body FROM attention_items WHERE id = ?`, id).
		Scan(&oldItemBody); err != nil {
		return readyReturnRewrite{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT body FROM attention_decision_surfaces WHERE item_id = ?`, id).
		Scan(&oldSurfaceBody); err != nil {
		return readyReturnRewrite{}, false, err
	}

	item.RequestedDecision = slices.Clone(productionReadyActions)
	item.ItemVersion++
	item.Recommendation = nil
	next, advanced, err := domain.NextDecisionSurface(surface, item)
	if err != nil || !advanced {
		return readyReturnRewrite{}, false, err
	}
	item.DecisionSurface = domain.DecisionSurfaceRef{Epoch: next.Epoch, Digest: next.Digest}
	if err := item.Validate(); err != nil {
		return readyReturnRewrite{}, false, err
	}
	return readyReturnRewrite{
		item: item, surface: next,
		entityVersion: snapshot.EntityVersion, asOfRevision: snapshot.AsOfRevision,
		oldItemBody: oldItemBody, oldSurfaceBody: oldSurfaceBody,
		oldSurfaceEpoch: surface.Epoch, oldSurfaceHash: surface.Digest,
	}, true, nil
}

func readyBindingForMigration(
	ctx context.Context,
	tx *sql.Tx,
	item domain.AttentionItem,
) (domain.ReadyItemPRBinding, bool, error) {
	itemID := item.ID
	var (
		storedItemID, storedRunID, producingID, publicationID string
		storedIdentity, recordedAt                            string
		repositoryID, prNumber                                int64
		body                                                  []byte
	)
	err := tx.QueryRowContext(ctx, getReadyItemPRBindingSQL, itemID).Scan(
		&storedItemID, &storedRunID, &producingID, &publicationID, &storedIdentity,
		&repositoryID, &prNumber, &body, &recordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReadyItemPRBinding{}, false, nil
	}
	if err != nil {
		return domain.ReadyItemPRBinding{}, false, err
	}
	binding, err := decode[domain.ReadyItemPRBinding](body)
	if err != nil || binding.ItemID != itemID || binding.ItemID != domain.ItemID(storedItemID) ||
		binding.RunID != domain.RunID(storedRunID) ||
		binding.ProducingInvocationID != domain.InvocationID(producingID) ||
		binding.PublicationInvocationID != domain.InvocationID(publicationID) ||
		binding.PublicationIdentity != domain.Digest(storedIdentity) ||
		binding.RepositoryID != repositoryID || int64(binding.PRNumber) != prNumber ||
		formatTime(binding.RecordedAt) != recordedAt {
		return domain.ReadyItemPRBinding{}, false, nil
	}
	reader := ReadTx{tx: tx}
	if err := reader.validateReadyItemPRBindingAgainst(ctx, item, binding); err != nil {
		return domain.ReadyItemPRBinding{}, false, nil
	}
	return binding, true, nil
}

func requireOneMigrationRow(result sql.Result) error {
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("updated %d rows, want 1", updated)
	}
	return nil
}
