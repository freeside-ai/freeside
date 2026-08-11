package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const attentionPRReferenceMigration = "0036_attention_pr_reference.sql"

func applyDataMigration(
	ctx context.Context,
	tx *sql.Tx,
	version int,
	name string,
) error {
	switch {
	case version == 36 && name == attentionPRReferenceMigration:
		if err := backfillLegacyFakePRReferences(ctx, tx); err != nil {
			return err
		}
		return rewriteAnchoredPRReferences(ctx, tx)
	case version == 39 && name == outboxPayloadAuthenticationMigration:
		return authenticateExistingOutboxPayloads(ctx, tx)
	default:
		return nil
	}
}

type legacyAttentionRow struct {
	id            domain.ItemID
	projectID     domain.ProjectID
	entityVersion int
	asOfRevision  int64
	body          []byte
	itemType      string
	status        string
}

func backfillLegacyFakePRReferences(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT i.id, i.project_id, i.entity_version,
		i.as_of_revision, i.body, i.item_type, i.status
		FROM attention_items AS i
		LEFT JOIN attention_item_pr_references AS r ON r.item_id = i.id
		WHERE i.item_type = 'ready_for_final_review'
		  AND json_type(i.body, '$.pr_reference') IS NULL
		  AND r.item_id IS NULL`)
	if err != nil {
		return fmt.Errorf("list legacy ready items: %w", err)
	}
	var candidates []legacyAttentionRow
	for rows.Next() {
		var row legacyAttentionRow
		if err := rows.Scan(
			&row.id, &row.projectID, &row.entityVersion, &row.asOfRevision,
			&row.body, &row.itemType, &row.status,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read legacy ready item: %w", err)
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read legacy ready items: %w", err)
	}

	for _, row := range candidates {
		reference, ok, err := authenticateLegacyFakePRReference(ctx, tx, row)
		if err != nil {
			return fmt.Errorf("authenticate legacy ready item %s: %w", row.id, err)
		}
		if !ok {
			continue
		}
		body, err := json.Marshal(reference)
		if err != nil {
			return fmt.Errorf("encode legacy ready item %s reference: %w", row.id, err)
		}
		if _, err := tx.ExecContext(ctx, insertAttentionItemPRReferenceSQL,
			row.id, reference.Repo, reference.Number, string(body)); err != nil {
			return fmt.Errorf("anchor legacy ready item %s: %w", row.id, err)
		}
	}
	return nil
}

func authenticateLegacyFakePRReference(
	ctx context.Context,
	tx *sql.Tx,
	row legacyAttentionRow,
) (domain.PRReference, bool, error) {
	var item domain.AttentionItem
	if err := decodeMigrationJSON(row.body, &item); err != nil {
		return domain.PRReference{}, false, nil
	}
	if item.ID != row.id || item.ProjectID != row.projectID ||
		item.ItemVersion != row.entityVersion || string(item.Type) != row.itemType ||
		string(item.Status) != row.status || item.Type != domain.AttentionReadyForFinalReview ||
		item.PRReference != nil || item.Subject.Type != domain.SubjectRun ||
		item.Subject.RunID == nil || *item.Subject.RunID == "" ||
		item.Subject.ID != domain.SubjectID(*item.Subject.RunID) ||
		row.id != fakepublication.ReadyItemID(*item.Subject.RunID) {
		return domain.PRReference{}, false, nil
	}

	taskKey := fakepublication.TaskKey(*item.Subject.RunID)
	var taskKind, taskStatus string
	var taskPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT kind, status, payload FROM outbox
		WHERE idempotency_key = ?`, taskKey).Scan(&taskKind, &taskStatus, &taskPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.PRReference{}, false, nil
		}
		return domain.PRReference{}, false, err
	}
	if taskKind != fakepublication.TaskKind || taskStatus != outboxStatusDispatched {
		return domain.PRReference{}, false, nil
	}
	task, err := fakepublication.DecodeTask(taskPayload)
	if err != nil || fakepublication.TaskKey(task.RunID) != taskKey ||
		task.RunID != *item.Subject.RunID || task.ProjectID != item.ProjectID {
		return domain.PRReference{}, false, nil
	}

	intentKey := "publish/" + string(task.PublicationInvocationID) + "/" + readyPublicationIntentKind
	var intentKind, intentStatus string
	var intentPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT kind, status, payload FROM outbox
		WHERE idempotency_key = ?`, intentKey).Scan(
		&intentKind, &intentStatus, &intentPayload,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.PRReference{}, false, nil
		}
		return domain.PRReference{}, false, err
	}
	intent, ok := decodeLegacyPublicationIntentForMigration(intentPayload)
	if !ok || intentKind != readyPublicationIntentKind ||
		intentStatus != outboxStatusDispatched ||
		intent.InvocationID != task.PublicationInvocationID ||
		intent.Repo != task.Repo || intent.BaseRef != task.BaseRef ||
		intent.SourceHeadSHA != item.PRHeadSHA || intent.ProducingInvocationID != "" ||
		intent.ReservationRunID != "" {
		return domain.PRReference{}, false, nil
	}

	outcomeKey := "publish.outcome/" + string(intent.Identity)
	var outcomeKind string
	var outcomePayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT kind, payload FROM inbox
		WHERE idempotency_key = ?`, outcomeKey).Scan(&outcomeKind, &outcomePayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.PRReference{}, false, nil
		}
		return domain.PRReference{}, false, err
	}
	outcome, err := decodeReadyPublicationOutcome(outcomePayload)
	if err != nil || outcomeKind != "publish.outcome" || outcome.Identity != intent.Identity ||
		outcome.Repo != intent.Repo || outcome.BaseRef != intent.BaseRef ||
		outcome.HeadSHA != intent.SourceHeadSHA {
		return domain.PRReference{}, false, nil
	}

	reference := domain.PRReference{Repo: outcome.Repo, Number: outcome.PRNumber}
	item.PRReference = &reference
	if err := item.Validate(); err != nil {
		return domain.PRReference{}, false, nil
	}
	if _, err := fakepublication.ValidateTerminalBinding(task, item); err != nil {
		return domain.PRReference{}, false, nil
	}
	return reference, true, nil
}

func rewriteAnchoredPRReferences(ctx context.Context, tx *sql.Tx) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM attention_items AS i
		JOIN attention_item_pr_references AS r ON r.item_id = i.id
		WHERE i.item_type = 'ready_for_final_review'
		  AND json_type(i.body, '$.pr_reference') IS NULL`).Scan(&count); err != nil {
		return fmt.Errorf("count anchored ready items: %w", err)
	}
	if count == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE server_state SET revision = revision + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("advance ready-item migration revision: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE attention_items AS i
		SET body = json_set(
			i.body,
			'$.pr_reference',
			json_object(
				'repo', (SELECT r.repo FROM attention_item_pr_references AS r WHERE r.item_id = i.id),
				'number', (SELECT r.pr_number FROM attention_item_pr_references AS r WHERE r.item_id = i.id)
			)
		),
		entity_version = entity_version + 1,
		as_of_revision = (SELECT revision FROM server_state WHERE id = 1)
		WHERE i.item_type = 'ready_for_final_review'
		  AND json_type(i.body, '$.pr_reference') IS NULL
		  AND EXISTS (SELECT 1 FROM attention_item_pr_references AS r WHERE r.item_id = i.id)`)
	if err != nil {
		return fmt.Errorf("rewrite anchored ready items: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count rewritten ready items: %w", err)
	}
	if updated != int64(count) {
		return fmt.Errorf("rewrote %d anchored ready items, want %d", updated, count)
	}
	return nil
}

func decodeMigrationJSON(payload []byte, value any) error {
	if err := strictjson.Decode(payload, value, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}
