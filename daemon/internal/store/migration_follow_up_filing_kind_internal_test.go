package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestFollowUpFilingKindMigrationPreservesRows proves migration 0088 rebuilds
// effect_proposal_instances on a store that already holds both earlier kinds
// and keeps every row byte for byte, along with the item binding and decision
// that reference one. The fixture carries a real run_proposal instance from
// before 0070; the source_issue_closure row is that row re-keyed, which is
// enough for a schema-level rebuild that never reads the body.
func TestFollowUpFilingKindMigrationPreservesRows(t *testing.T) {
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
	migrateThrough(t, ctx, db, "0088_")

	const taskInstance = "proposal-dcd53aaaa960fd8e8fbe2339a17c02f3"
	const closureInstance = "proposal-closure-fixture"
	if _, err := db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
		(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
		 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
		SELECT ?, 'effect-proposal/closure-fixture', proposal_batch_id, 'source_issue_closure',
		       content_digest, resolved_policy_run_id, resolved_policy_digest, subject_handle,
		       created_at, body
		FROM effect_proposal_instances WHERE instance_id = ?`, closureInstance, taskInstance); err != nil {
		t.Fatalf("seed the closure row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
		(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
		 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
		SELECT 'proposal-filing-early', 'effect-proposal/filing-early', proposal_batch_id,
		       'follow_up_filing', content_digest, resolved_policy_run_id, resolved_policy_digest,
		       subject_handle, created_at, body
		FROM effect_proposal_instances WHERE instance_id = ?`, taskInstance); err == nil {
		t.Fatal("the prior schema admitted a follow_up_filing row")
	}

	rows := func() []string {
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
	dependents := func() [2]int {
		t.Helper()
		var counts [2]int
		for i, table := range []string{"effect_proposal_items", "effect_proposal_decisions"} {
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM `+table+` WHERE instance_id = ?`, taskInstance).Scan(&counts[i]); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
		}
		return counts
	}
	before, dependentsBefore := rows(), dependents()
	if len(before) != 2 || dependentsBefore != [2]int{1, 1} {
		t.Fatalf("fixture holds %d instances and dependents %v, want 2 and [1 1]", len(before), dependentsBefore)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)

	if after := rows(); !slices.Equal(after, before) {
		t.Fatalf("instances after the rebuild = %q, want %q", after, before)
	}
	if after := dependents(); after != dependentsBefore {
		t.Fatalf("dependents after the rebuild = %v, want %v", after, dependentsBefore)
	}
	var violations int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("rebuild left %d foreign-key violations", violations)
	}

	insert := func(id, kind string) (sql.Result, error) {
		return db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
			(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
			 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
			SELECT ?, 'effect-proposal/' || ?, proposal_batch_id, ?, content_digest,
			       resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body
			FROM effect_proposal_instances WHERE instance_id = ?`, id, id, kind, taskInstance)
	}
	if _, err := insert("proposal-filing", "follow_up_filing"); err != nil {
		t.Fatalf("the rebuilt CHECK refused follow_up_filing: %v", err)
	}
	if _, err := insert("proposal-unknown", "unregistered_kind"); err == nil {
		t.Fatal("the rebuilt CHECK admitted an unregistered kind")
	}
}
