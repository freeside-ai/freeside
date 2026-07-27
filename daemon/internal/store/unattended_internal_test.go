package store

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestUnattendedOperationMigrationAppliesFromHead is the migration acceptance
// for 0017: a database at the real prior head upgrades with its existing
// attention items intact, the backfill derives item_type/status from each
// stored body, and the new transition log starts empty — the legitimate
// "never stopped" state, so nothing about the upgrade closes admission.
func TestUnattendedOperationMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0017_")

	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "diagnostic finding",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items
		   (id, project_id, conversation_id, entity_version, as_of_revision, body)
		 VALUES ('item-1', 'proj-1', NULL, 1, 1, ?)`, body); err != nil {
		t.Fatalf("seed attention item: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var itemType, status string
	if err := db.QueryRowContext(ctx,
		`SELECT item_type, status FROM attention_items WHERE id = 'item-1'`).
		Scan(&itemType, &status); err != nil {
		t.Fatalf("read backfilled columns: %v", err)
	}
	if itemType != string(domain.AttentionSystemHealth) || status != string(domain.StatusOpen) {
		t.Fatalf("backfilled columns = (%q, %q), want (system_health, open)", itemType, status)
	}

	var transitions int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM unattended_operation_transitions`).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if transitions != 0 {
		t.Fatalf("transition log after upgrade holds %d rows, want 0", transitions)
	}
}

// TestForgedItemTypeColumnFailsClosed pins the lookup-key trust rule: the
// extracted columns select candidates, but a row whose columns diverge from
// its canonical body is refused at reconstruction, so a tampered column
// cannot hide an open blocking item from the admission query while the row
// still reads as valid elsewhere.
func TestForgedItemTypeColumnFailsClosed(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "diagnostic finding",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for name, forged := range map[string]string{
		"item_type": `INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body)
		 VALUES ('item-1', 'proj-1', NULL, 'blocked', 'open', 1, 1, ?)`,
		"status": `INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body)
		 VALUES ('item-1', 'proj-1', NULL, 'system_health', 'resolved', 1, 1, ?)`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM attention_items`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx, forged, body); err != nil {
				t.Fatalf("seed forged row: %v", err)
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			rtx := ReadTx{tx: tx}
			if _, err := rtx.GetAttentionItem(ctx, "item-1"); err == nil {
				t.Fatal("forged column reconstructed as valid")
			}
		})
	}
}
