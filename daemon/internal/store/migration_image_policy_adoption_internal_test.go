package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestImagePolicyAdoptionMigrationPreservesRows proves migration 0110
// rebuilds effect_proposal_instances on a store that already holds cards and
// keeps every row byte for byte, along with the item binding and decision
// that reference one. The rebuilt table admits the adoption kind with no
// resolved policy and still requires both policy columns of every other kind.
func TestImagePolicyAdoptionMigrationPreservesRows(t *testing.T) {
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
	migrateThrough(t, ctx, db, "0110_")

	const taskInstance = "proposal-dcd53aaaa960fd8e8fbe2339a17c02f3"
	insert := func(id, kind string, policy bool) error {
		_, err := db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
			(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
			 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
			SELECT ?, 'effect-proposal/' || ?, proposal_batch_id, ?, content_digest,
			       CASE WHEN ? THEN resolved_policy_run_id END,
			       CASE WHEN ? THEN resolved_policy_digest END,
			       subject_handle, created_at, body
			FROM effect_proposal_instances WHERE instance_id = ?`, id, id, kind, policy, policy, taskInstance)
		return err
	}
	if err := insert("proposal-filing-fixture", "follow_up_filing", true); err != nil {
		t.Fatalf("seed the filing row: %v", err)
	}
	if err := insert("proposal-adoption-early", "image_policy_adoption", false); err == nil {
		t.Fatal("the prior schema admitted an image_policy_adoption row")
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

	for _, tc := range []struct {
		id, kind string
		policy   bool
		admitted bool
	}{
		{"proposal-adoption", "image_policy_adoption", false, true},
		{"proposal-adoption-policy", "image_policy_adoption", true, false},
		{"proposal-filing-no-policy", "follow_up_filing", false, false},
		{"proposal-unknown", "unregistered_kind", true, false},
	} {
		if err := insert(tc.id, tc.kind, tc.policy); (err == nil) != tc.admitted {
			t.Errorf("insert %s with policy %v: err = %v, want admitted %v", tc.kind, tc.policy, err, tc.admitted)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
		(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
		 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
		SELECT 'proposal-duplicate-key', admission_key, proposal_batch_id, effect_kind, content_digest,
		       resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body
		FROM effect_proposal_instances WHERE instance_id = ?`, taskInstance); err == nil {
		t.Fatal("the rebuilt table admitted a duplicate admission_key")
	}
}
