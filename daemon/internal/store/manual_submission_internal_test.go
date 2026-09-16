package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestManualSubmissionMigrationPreservesHistoricalState(t *testing.T) {
	ctx := t.Context()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0070_")
	dump, err := os.ReadFile(filepath.Join("testdata", "pre_tasks_started_label_intake.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(dump)); err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, ctx, db, "0075_")
	var taskID, projectID, runID string
	if err := db.QueryRowContext(ctx, `SELECT task_id, project_id, id FROM runs ORDER BY id LIMIT 1`).Scan(&taskID, &projectID, &runID); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"old-a", "old-b"} {
		// Two old commands can share a task and result name. The requested name
		// was not stored and must not be reconstructed from that display name.
		body, err := json.Marshal(map[string]any{
			"command_id": command, "device_id": "old-device",
			"project_id": projectID, "task_id": taskID, "specification_run_id": runID,
			"source_digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"name":          map[string]string{"text": "Original display name", "source": "operator"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO task_submission_commands
			(command_id, device_id, project_id, task_id, specification_run_id, entity_version, as_of_revision, body)
			VALUES (?, 'old-device', ?, ?, ?, 1, 17, ?)`, command, projectID, taskID, runID, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	queries := []string{
		`SELECT * FROM tasks ORDER BY id`, `SELECT * FROM task_intake_keys ORDER BY task_id`,
		`SELECT * FROM runs ORDER BY id`, `SELECT * FROM production_attempts ORDER BY campaign_id, attempt_number`,
		`SELECT * FROM attention_items ORDER BY id`, `SELECT * FROM commands ORDER BY command_id`,
		`SELECT * FROM outbox ORDER BY rowid`,
		`SELECT command_id, device_id, project_id, task_id, specification_run_id, entity_version, as_of_revision, body FROM task_submission_commands ORDER BY command_id`,
	}
	before := make([][][]any, len(queries))
	for i, q := range queries {
		before[i] = manualMigrationRows(t, db, q)
	}
	revision := serverRevision(t, ctx, db)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	for i, q := range queries {
		if !reflect.DeepEqual(before[i], manualMigrationRows(t, db, q)) {
			t.Fatalf("migration changed historical rows: %s", q)
		}
	}
	if serverRevision(t, ctx, db) != revision {
		t.Fatal("additive migration advanced existing results")
	}
	var fingerprintCount, submissionCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM task_submission_commands WHERE request_digest IS NOT NULL`).Scan(&fingerprintCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM manual_submissions`).Scan(&submissionCount); err != nil {
		t.Fatal(err)
	}
	if fingerprintCount != 0 || submissionCount != 0 {
		t.Fatal("migration fabricated historical submission metadata")
	}
}

func manualMigrationRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := [][]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
