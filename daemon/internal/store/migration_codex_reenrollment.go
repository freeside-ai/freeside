package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const codexReenrollmentMigration = "0040_codex_reenrollment.sql"

type legacyCodexReenrollmentRow struct {
	id            domain.ItemID
	projectID     domain.ProjectID
	itemType      string
	status        string
	healthPosture sql.NullString
	entityVersion int64
	asOfRevision  int64
	body          []byte
}

type legacyCodexReenrollmentRewrite struct {
	row  legacyCodexReenrollmentRow
	body []byte
}

func rewriteLegacyCodexReenrollmentMarkers(ctx context.Context, tx *sql.Tx) error {
	identities, err := listCodexReenrollmentMigrationIdentities(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, project_id, item_type, status,
		health_posture, entity_version, as_of_revision, body
		FROM attention_items WHERE item_type = 'system_health' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list legacy codex re-enrollment markers: %w", err)
	}
	var candidates []legacyCodexReenrollmentRow
	for rows.Next() {
		var row legacyCodexReenrollmentRow
		if err := rows.Scan(
			&row.id, &row.projectID, &row.itemType, &row.status, &row.healthPosture,
			&row.entityVersion, &row.asOfRevision, &row.body,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read legacy codex re-enrollment marker: %w", err)
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read legacy codex re-enrollment markers: %w", err)
	}

	var rewrites []legacyCodexReenrollmentRewrite
	for _, row := range candidates {
		updated, ok := authenticateLegacyCodexReenrollmentMarker(row, identities)
		if !ok {
			continue
		}
		body, err := json.Marshal(updated)
		if err != nil {
			return fmt.Errorf("encode legacy codex re-enrollment marker %s: %w", row.id, err)
		}
		rewrites = append(rewrites, legacyCodexReenrollmentRewrite{row: row, body: body})
	}
	if len(rewrites) == 0 {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE server_state SET revision = revision + 1 WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("advance codex re-enrollment migration revision: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count codex re-enrollment migration revision update: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("advance codex re-enrollment migration revision: updated %d rows, want 1", updated)
	}
	for _, rewrite := range rewrites {
		row := rewrite.row
		result, err := tx.ExecContext(ctx, `UPDATE attention_items
			SET body = ?, entity_version = entity_version + 1,
			    as_of_revision = (SELECT revision FROM server_state WHERE id = 1)
			WHERE id = ? AND project_id = ? AND item_type = ? AND status = ?
			  AND health_posture IS ? AND entity_version = ? AND as_of_revision = ? AND body = ?`,
			string(rewrite.body), row.id, row.projectID, row.itemType, row.status,
			row.healthPosture, row.entityVersion, row.asOfRevision, string(row.body))
		if err != nil {
			return fmt.Errorf("rewrite legacy codex re-enrollment marker %s: %w", row.id, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count rewritten codex re-enrollment marker %s: %w", row.id, err)
		}
		if updated != 1 {
			return fmt.Errorf("rewrote %d legacy codex re-enrollment marker %s rows, want 1", updated, row.id)
		}
	}
	return nil
}

func listCodexReenrollmentMigrationIdentities(
	ctx context.Context, tx *sql.Tx,
) ([]domain.AuthIdentityID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM auth_identities ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list codex re-enrollment migration identities: %w", err)
	}
	var identities []domain.AuthIdentityID
	for rows.Next() {
		var id domain.AuthIdentityID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read codex re-enrollment migration identity: %w", err)
		}
		identities = append(identities, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read codex re-enrollment migration identities: %w", err)
	}
	return identities, nil
}

func authenticateLegacyCodexReenrollmentMarker(
	row legacyCodexReenrollmentRow, identities []domain.AuthIdentityID,
) (domain.AttentionItem, bool) {
	var item domain.AttentionItem
	if err := decodeMigrationJSON(row.body, &item); err != nil ||
		item.ID != row.id || item.ProjectID != row.projectID ||
		string(item.Type) != row.itemType || string(item.Status) != row.status ||
		!row.healthPosture.Valid || item.Posture == nil ||
		string(*item.Posture) != row.healthPosture.String {
		return domain.AttentionItem{}, false
	}
	for _, id := range identities {
		occurrence, ok := canonicalCodexReenrollmentOccurrence(row.id, id)
		if !ok {
			continue
		}
		expected, err := legacyCodexReenrollmentMarker(
			id, item.ID, item.ProjectID, item.ItemVersion, item.Status,
		)
		if err != nil {
			return domain.AttentionItem{}, false
		}
		expected.Timing = item.Timing
		expected.DecidedAt = item.DecidedAt
		if !reflect.DeepEqual(expected, item) {
			continue
		}
		updated, err := NewCodexReenrollmentMarker(
			id, occurrence, item.ProjectID, item.ItemVersion, item.Status, nil,
		)
		if err != nil {
			return domain.AttentionItem{}, false
		}
		updated.Timing = item.Timing
		updated.DecidedAt = item.DecidedAt
		if err := updated.Validate(); err != nil {
			return domain.AttentionItem{}, false
		}
		return updated, true
	}
	return domain.AttentionItem{}, false
}

func canonicalCodexReenrollmentOccurrence(
	itemID domain.ItemID, id domain.AuthIdentityID,
) (int, bool) {
	digest := contentaddr.Hex(contentaddr.Sum([]byte(id)))
	prefix := "system-health-codex-auth-" + digest + "-"
	suffix := strings.TrimPrefix(string(itemID), prefix)
	if suffix == string(itemID) {
		return 0, false
	}
	occurrence, err := strconv.Atoi(suffix)
	return occurrence, err == nil && occurrence > 0 && suffix == strconv.Itoa(occurrence)
}

func legacyCodexReenrollmentMarker(
	id domain.AuthIdentityID,
	itemID domain.ItemID,
	projectID domain.ProjectID,
	version int,
	status domain.ItemStatus,
) (domain.AttentionItem, error) {
	posture := domain.HealthPostureAdvisory
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: projectID,
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Codex auth identity %q can no longer refresh. Re-enroll the identity, then acknowledge this item before retrying.",
			id,
		),
		RequestedDecision: []domain.Action{
			domain.ActionAcknowledge, domain.ActionStopUnattended,
		},
		ItemVersion: version, InterruptionClass: domain.InterruptionExceptional,
		Posture: &posture, Status: status,
	}, nil)
}
