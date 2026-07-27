package store

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestCommandBackupBindingMigrationAppliesFromHead is the migration acceptance
// for 0015: existing commands survive, and their unknown inline classification
// is left empty so backup closure fails conservatively.
func TestCommandBackupBindingMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0015_")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items
		   (id, project_id, conversation_id, entity_version, as_of_revision, body)
		 VALUES ('item-1', 'proj-1', NULL, 1, 1, '{}')`); err != nil {
		t.Fatalf("seed attention item: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO commands
		   (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body)
		 VALUES ('cmd-1', 'item-1', 1, 'cafebabe', 'device-1', 'open_pr', 1, 1, '{}')`); err != nil {
		t.Fatalf("seed command: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var commands int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM commands WHERE command_id = 'cmd-1'`).Scan(&commands); err != nil {
		t.Fatalf("count commands: %v", err)
	}
	if commands != 1 {
		t.Errorf("pre-migration command count = %d, want 1", commands)
	}

	var backupBindingDigest string
	if err := db.QueryRowContext(ctx,
		`SELECT backup_binding_digest FROM commands WHERE command_id = 'cmd-1'`).
		Scan(&backupBindingDigest); err != nil {
		t.Fatalf("read legacy command backup binding: %v", err)
	}
	if backupBindingDigest != "" {
		t.Errorf("legacy command backup binding = %q, want empty", backupBindingDigest)
	}
	var checkpointMarkers int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM local_backup_checkpoint_marker`).Scan(&checkpointMarkers); err != nil {
		t.Fatalf("count checkpoint markers: %v", err)
	}
	if checkpointMarkers != 0 {
		t.Errorf("checkpoint markers = %d, want 0", checkpointMarkers)
	}
	var restoreMarkers int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM local_backup_restore_marker`).Scan(&restoreMarkers); err != nil {
		t.Fatalf("count restore markers: %v", err)
	}
	if restoreMarkers != 0 {
		t.Errorf("restore markers = %d, want 0", restoreMarkers)
	}
}
