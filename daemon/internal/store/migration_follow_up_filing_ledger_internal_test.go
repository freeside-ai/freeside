package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestFollowUpFilingLedgerMigrationAppliesToExistingStore proves migration
// 0089 lands on a store that already holds proposal instances: every
// instance row survives byte for byte, the three ledger tables start empty,
// and they then accept rows that reference a preserved instance.
func TestFollowUpFilingLedgerMigrationAppliesToExistingStore(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)

	migrateThrough(t, ctx, db, "0070_")
	dump, err := os.ReadFile(filepath.Join("testdata", "pre_tasks_started_label_intake.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(dump)); err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, ctx, db, "0089_")

	const taskInstance = "proposal-dcd53aaaa960fd8e8fbe2339a17c02f3"
	const filingInstance = "proposal-filing-fixture"
	// The filing row is the task row re-keyed, which is enough for a
	// migration that never reads the body.
	if _, err := db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
		(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
		 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
		SELECT ?, 'effect-proposal/filing-fixture', proposal_batch_id, 'follow_up_filing',
		       content_digest, resolved_policy_run_id, resolved_policy_digest, subject_handle,
		       created_at, body
		FROM effect_proposal_instances WHERE instance_id = ?`, filingInstance, taskInstance); err != nil {
		t.Fatalf("seed the filing row: %v", err)
	}
	instances := func() []string {
		t.Helper()
		result, err := db.QueryContext(ctx, `SELECT instance_id || '|' || admission_key || '|' ||
			proposal_batch_id || '|' || effect_kind || '|' || content_digest || '|' ||
			resolved_policy_run_id || '|' || resolved_policy_digest || '|' || subject_handle || '|' ||
			created_at || '|' || body FROM effect_proposal_instances ORDER BY instance_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = result.Close() }()
		var out []string
		for result.Next() {
			var row string
			if err := result.Scan(&row); err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		if err := result.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := instances()
	if len(before) != 2 {
		t.Fatalf("fixture holds %d instances, want 2", len(before))
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)

	if after := instances(); !slices.Equal(after, before) {
		t.Fatalf("instances after the migration = %q, want %q", after, before)
	}
	for _, table := range ledgerTables {
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != 0 {
			t.Fatalf("%s starts with %d rows, want none", table, rows)
		}
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO follow_up_filing_intents
		(instance_id, repository_id, opened_at) VALUES (?, 123, '2026-10-05T12:00:00Z')`, filingInstance); err != nil {
		t.Fatalf("intent for a preserved instance: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO follow_up_filing_intents
		(instance_id, repository_id, opened_at) VALUES (?, 123, '2026-10-05T12:00:00Z')`, taskInstance); err == nil {
		t.Fatal("the migrated schema admitted a second outstanding intent in one repository")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO follow_up_filing_intents
		(instance_id, repository_id, opened_at) VALUES ('proposal-missing', 456, '2026-10-05T12:00:00Z')`); err == nil {
		t.Fatal("the migrated schema admitted an intent for no proposal instance")
	}
	var violations int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("migration left %d foreign-key violations", violations)
	}
}
