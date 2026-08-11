package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

const migratedLegacyPublicationKey = "publish/legacy-publication/publish.publication"

func TestOutboxPayloadAuthenticationMigrationMarksLegacyAndQuarantinesInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0039_")
	legacyPayload, err := json.Marshal(map[string]any{
		"identity":         "sha256:01c663f9a986e10d214b2c31c75fa5088e2995674a8e8f2ba959111e06a23fb8",
		"invocation_id":    "legacy-publication",
		"repo":             "freeside-ai/evidence-repo",
		"base_ref":         "main",
		"source_head_sha":  "6dcb09b5b57875f334f61aebed695e2e4193db5e",
		"authorization_id": "sha256:02c663f9a986e10d214b2c31c75fa5088e2995674a8e8f2ba959111e06a23fb8",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		key     string
		payload []byte
	}{
		{migratedLegacyPublicationKey, legacyPayload},
		{"publish/corrupt-publication/publish.publication", []byte(`{"broken":true}`)},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO outbox
			(idempotency_key, kind, payload, created_at) VALUES (?, 'publish.publication', ?, '2026-08-11T12:00:00Z')`,
			row.key, row.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	var legacy, corrupt QueueEntry
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		legacy, err = tx.GetOutbox(ctx, migratedLegacyPublicationKey)
		if err != nil {
			return err
		}
		corrupt, err = tx.GetOutbox(ctx, "publish/corrupt-publication/publish.publication")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	intent, err := publicationrecord.DecodeIntent(legacy.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.PayloadVersion != publicationrecord.IntentFormatLegacy ||
		intent.FormatVersion != publicationrecord.IntentFormatLegacy || corrupt.PayloadVersion != 1 ||
		!corrupt.Quarantined() {
		t.Fatalf("legacy = %+v, intent = %+v, corrupt = %+v", legacy, intent, corrupt)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO outbox
		(idempotency_key, kind, payload, payload_version, payload_digest, created_at)
		VALUES ('publish/forged-legacy/publish.publication', 'publish.publication',
		'{}', 1, 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		'2026-08-11T12:00:00Z')`); err == nil {
		t.Fatal("schema accepted a new legacy publication intent")
	}
	if _, err := db.ExecContext(ctx, `UPDATE outbox
		SET payload = CAST(json_remove(payload, '$.format_version') AS BLOB)
		WHERE idempotency_key = 'publish/legacy-publication/publish.publication'`); err != nil {
		t.Fatal(err)
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetOutbox(ctx, migratedLegacyPublicationKey)
		return err
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("tampered migrated payload error = %v, want parent-key mismatch", err)
	}
}

func TestPlaintextRestorePreservesMigratedLegacyPublicationIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedLegacyPublicationStore(t, ctx)
	checkpoint := filepath.Join(t.TempDir(), "legacy-checkpoint.db")
	if err := s.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	deleteMigratedLegacyPublication(t, ctx, s)
	if _, err := s.Restore(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	assertMigratedLegacyPublication(t, ctx, s)
}

func TestEncryptedRestorePathPreservesMigratedLegacyPublicationIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedLegacyPublicationStore(t, ctx)
	plaintext, err := serializeStoreCheckpoint(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB, source, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDeserializedBackupDatabase(sourceDB, source)
	deleteMigratedLegacyPublication(t, ctx, s)
	if _, err := s.restoreFromDatabase(ctx, source); err != nil {
		t.Fatal(err)
	}
	assertMigratedLegacyPublication(t, ctx, s)
}

func TestRestoreRejectsWeakenedPublicationInsertGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := migratedLegacyPublicationStore(t, ctx)
	checkpoint := filepath.Join(t.TempDir(), "canonical-trigger-checkpoint.db")
	if err := s.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER outbox_publication_intent_requires_current_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER outbox_publication_intent_requires_current_insert
		BEFORE INSERT ON outbox
		WHEN 0
		BEGIN
			SELECT RAISE(ABORT, 'disabled publication guard');
		END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, checkpoint); err == nil ||
		!strings.Contains(err.Error(), "guard definition is not canonical") {
		t.Fatalf("restore with weakened publication guard = %v, want canonical-definition refusal", err)
	}
}

func migratedLegacyPublicationStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0039_")
	payload, err := json.Marshal(map[string]any{
		"identity":         "sha256:01c663f9a986e10d214b2c31c75fa5088e2995674a8e8f2ba959111e06a23fb8",
		"invocation_id":    "legacy-publication",
		"repo":             "freeside-ai/evidence-repo",
		"base_ref":         "main",
		"source_head_sha":  "6dcb09b5b57875f334f61aebed695e2e4193db5e",
		"authorization_id": "sha256:02c663f9a986e10d214b2c31c75fa5088e2995674a8e8f2ba959111e06a23fb8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO outbox
		(idempotency_key, kind, payload, created_at)
		VALUES (?, 'publish.publication', ?, '2026-08-11T12:00:00Z')`,
		migratedLegacyPublicationKey, payload); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return &Store{db: db}
}

func deleteMigratedLegacyPublication(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(
		ctx, "DELETE FROM outbox WHERE idempotency_key = ?", migratedLegacyPublicationKey,
	); err != nil {
		t.Fatal(err)
	}
}

func assertMigratedLegacyPublication(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	if err := s.Read(ctx, func(tx *ReadTx) error {
		entry, err := tx.GetOutbox(ctx, migratedLegacyPublicationKey)
		if err != nil {
			return err
		}
		if entry.PayloadVersion != publicationrecord.IntentFormatLegacy {
			t.Errorf("restored payload version = %d, want legacy", entry.PayloadVersion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
