package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestDeviceNotificationPreferencesMigrationWritesNoRows proves migration
// 0131 leaves every device paired before it off: the table starts empty, so
// an existing device has no switch until an operator turns it on.
func TestDeviceNotificationPreferencesMigrationWritesNoRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	copySnapshotThrough(t, path, "0131_")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(domain.Device{
		ID: "device-1", DisplayName: "Ben's iPhone", Status: domain.DeviceActive,
		PairedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO devices (id, status, entity_version, as_of_revision, body) VALUES ('device-1', 'active', 1, 1, ?)`,
		string(body)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening the store runs 0131 and every migration after it.
	s := openTemplateStoreAt(t, path, Options{})
	if err := s.Read(ctx, func(tx *ReadTx) error {
		prefs, err := tx.ListDeviceNotificationPreferences(ctx)
		if err != nil {
			return err
		}
		if len(prefs) != 0 {
			t.Errorf("preferences after migration = %#v, want none", prefs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// rewindDeviceNotificationPreferences returns a migrated store to the schema
// 0130 leaves: no preference table.
func rewindDeviceNotificationPreferences(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindAttentionItemViews(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE device_notification_preferences`,
		`DELETE FROM schema_migrations WHERE name = '0131_device_notification_preferences.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
