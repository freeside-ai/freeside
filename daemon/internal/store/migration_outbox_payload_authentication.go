package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

const outboxPayloadAuthenticationMigration = "0039_outbox_payload_authentication.sql"

type outboxPayloadMigrationRow struct {
	id      int64
	kind    string
	payload []byte
}

func authenticateExistingOutboxPayloads(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, payload FROM outbox ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list outbox payloads: %w", err)
	}
	var existing []outboxPayloadMigrationRow
	for rows.Next() {
		var row outboxPayloadMigrationRow
		if err := rows.Scan(&row.id, &row.kind, &row.payload); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read outbox payload: %w", err)
		}
		existing = append(existing, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("read outbox payloads: %w", err)
	}

	for _, row := range existing {
		payload := row.payload
		status := ""
		if row.kind == readyPublicationIntentKind {
			var ok bool
			payload, ok = migrateLegacyPublicationIntent(payload)
			if !ok {
				payload = row.payload
				status = outboxStatusQuarantined
			}
		}
		digest := contentaddr.Sum(payload)
		var result sql.Result
		if status == "" {
			result, err = tx.ExecContext(ctx, `UPDATE outbox
				SET payload = ?, payload_version = 1, payload_digest = ? WHERE id = ?`,
				payload, digest, row.id)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE outbox
				SET payload = ?, payload_version = 1, payload_digest = ?, status = ? WHERE id = ?`,
				payload, digest, status, row.id)
		}
		if err != nil {
			return fmt.Errorf("authenticate outbox payload %d: %w", row.id, err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("authenticate outbox payload %d: %w", row.id, err)
		}
		if updated != 1 {
			return fmt.Errorf("authenticate outbox payload %d: updated %d rows, want 1", row.id, updated)
		}
	}
	return nil
}

func migrateLegacyPublicationIntent(payload []byte) ([]byte, bool) {
	intent, ok := decodeLegacyPublicationIntentForMigration(payload)
	if !ok {
		return nil, false
	}
	canonical, err := intent.Encode()
	if err != nil {
		return nil, false
	}
	return canonical, true
}

func decodeLegacyPublicationIntentForMigration(payload []byte) (publicationrecord.Intent, bool) {
	var raw map[string]json.RawMessage
	if err := decodeMigrationJSON(payload, &raw); err != nil {
		return publicationrecord.Intent{}, false
	}
	if _, exists := raw["format_version"]; exists {
		return publicationrecord.Intent{}, false
	}
	raw["format_version"] = json.RawMessage("1")
	upgraded, err := json.Marshal(raw)
	if err != nil {
		return publicationrecord.Intent{}, false
	}
	intent, err := publicationrecord.DecodeIntent(upgraded)
	if err != nil {
		return publicationrecord.Intent{}, false
	}
	return intent, true
}
