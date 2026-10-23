package store

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestUsageObservationsReviewMigrationKeepsRows is the 0102 acceptance: a
// usage row recorded under 0061's schema survives the rebuild unchanged, and
// the rebuilt table keeps its append-only guard while no longer requiring an
// execution admission.
func TestUsageObservationsReviewMigrationKeepsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0102_")

	const row = `'inv-1', 'run-1', 'sha256:agent', 'sha256:launch', 'sha256:treatment',
		'pricing-1', 'adapter_transcript', 'reported_usage', 'input_tokens', 'tokens',
		123, 1, '2026-01-02T04:05:06Z'`
	for _, statement := range []string{
		`INSERT INTO tasks (id, project_id, entity_version, as_of_revision, body)
		 VALUES ('task-1', 'proj-1', 1, 1, '{}')`,
		`INSERT INTO runs (id, project_id, task_id, policy_digest, entity_version, as_of_revision, body)
		 VALUES ('run-1', 'proj-1', 'task-1', 'sha256:policy', 1, 1, '{}')`,
		`INSERT INTO execution_admissions
		 (invocation_id, id, run_id, stage_id, attempt_id, operating_mode, admitted_at, body)
		 VALUES ('inv-1', 'adm-1', 'run-1', 'stage-1', 'attempt-1', 'unattended',
		 '2026-01-02T04:05:06Z', '{}')`,
		`INSERT INTO usage_observations
		 (invocation_id, run_id, agent_digest, launch_digest, treatment_digest,
		  pricing_revision, source, kind, metric, unit, quantity, sequence, observed_at)
		 VALUES (` + row + `)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed under the prior schema: %v", err)
		}
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM usage_observations
		WHERE (invocation_id, run_id, agent_digest, launch_digest, treatment_digest,
		  pricing_revision, source, kind, metric, unit, quantity, sequence, observed_at)
		  = (`+row+`)`).Scan(&count); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if count != 1 {
		t.Fatalf("migrated rows matching the seed = %d, want 1", count)
	}
	if _, err := db.ExecContext(ctx, `UPDATE usage_observations SET quantity = 0`); err == nil {
		t.Fatal("update after the rebuild succeeded, want the append-only guard")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM usage_observations`); err == nil {
		t.Fatal("delete after the rebuild succeeded, want the append-only guard")
	}
	// A review invocation has no execution admission; the rebuilt table
	// accepts its row, and attribution stays the store function's gate.
	if _, err := db.ExecContext(ctx, `INSERT INTO usage_observations
		(invocation_id, run_id, agent_digest, launch_digest, treatment_digest,
		 pricing_revision, source, kind, metric, unit, quantity, sequence, observed_at)
		VALUES ('review-1', 'run-1', 'sha256:agent', 'sha256:launch', 'sha256:treatment',
		 'pricing-1', 'review_source', 'reported_usage', 'input_tokens', 'tokens',
		 7, 1, '2026-01-02T04:05:06Z')`); err != nil {
		t.Fatalf("insert a row with no execution admission: %v", err)
	}
}
