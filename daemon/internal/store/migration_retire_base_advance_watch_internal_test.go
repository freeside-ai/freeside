package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestRetireBaseAdvanceWatchMigrationDeletesOnlyTheKind proves migration 0128
// deletes every base_advance_watch schedule, armed or resolved, with its
// timer and occurrences, leaves another kind's rows byte-identical, and
// advances the sync cursor once only when it deleted something.
func TestRetireBaseAdvanceWatchMigrationDeletesOnlyTheKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		withWatch bool
	}{
		{name: "with watch rows", withWatch: true},
		{name: "without watch rows", withWatch: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openRaw(t)
			migrateThrough(t, ctx, db, "0128_")
			if err := seedEpoch(ctx, db); err != nil {
				t.Fatal(err)
			}
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.ExecContext(ctx, query, args...); err != nil {
					t.Fatalf("%s: %v", query, err)
				}
			}
			if err := insertRawRunFixture(ctx, db, "run-1", "project-1", "sha256:policy", 1, 1, `{}`); err != nil {
				t.Fatal(err)
			}
			insertSchedule := func(id, kind, status string, fireAt any) {
				t.Helper()
				exec(`INSERT INTO schedules
					(id, project_id, kind, status, generation, run_id, policy_digest,
					 fire_at, entity_version, as_of_revision, body)
					VALUES (?, 'project-1', ?, ?, 1, 'run-1', 'sha256:policy', ?, 1, 1, ?)`,
					id, kind, status, fireAt, `{"id":"`+id+`"}`)
			}
			insertSchedule("schedule-pr_checks_deadline-item-1", "pr_checks_deadline", "armed", int64(1000))
			exec(`INSERT INTO schedule_occurrences
				(schedule_id, generation, nominal_fire_at, status, created_at)
				VALUES ('schedule-pr_checks_deadline-item-1', 1, 1000, 'pending', '2026-11-05T00:00:00Z')`)
			if tc.withWatch {
				insertSchedule("schedule-base_advance_watch-item-1", "base_advance_watch", "armed", nil)
				exec(`INSERT INTO schedule_timers (schedule_id, generation, next_nominal_fire_at)
					VALUES ('schedule-base_advance_watch-item-1', 1, 2000)`)
				exec(`INSERT INTO schedule_occurrences
					(schedule_id, generation, nominal_fire_at, status, created_at)
					VALUES ('schedule-base_advance_watch-item-1', 1, 2000, 'pending', '2026-11-05T00:00:00Z')`)
				insertSchedule("schedule-base_advance_watch-item-2", "base_advance_watch", "resolved", nil)
				exec(`INSERT INTO schedule_occurrences
					(schedule_id, generation, nominal_fire_at, status, created_at, consumed_at, outcome)
					VALUES ('schedule-base_advance_watch-item-2', 1, 2000, 'consumed',
						'2026-11-05T00:00:00Z', '2026-11-05T00:01:00Z', 'condition_no_longer_applies')`)
			}
			beforeDeadline := scheduleRowText(t, ctx, db, "schedule-pr_checks_deadline-item-1")
			beforeRevision := serverRevision(t, ctx, db)

			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			assertAtHead(t, db)

			for table, want := range map[string]int{
				"schedules": 1, "schedule_timers": 0, "schedule_occurrences": 1,
			} {
				var got int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("%s rows = %d, want %d", table, got, want)
				}
			}
			var kinds int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schedule_occurrences
				WHERE schedule_id <> 'schedule-pr_checks_deadline-item-1'`).Scan(&kinds); err != nil {
				t.Fatal(err)
			}
			if kinds != 0 {
				t.Fatalf("watch occurrences left = %d", kinds)
			}
			if after := scheduleRowText(t, ctx, db, "schedule-pr_checks_deadline-item-1"); after != beforeDeadline {
				t.Fatalf("deadline row changed:\n got %s\nwant %s", after, beforeDeadline)
			}
			wantRevision := beforeRevision
			if tc.withWatch {
				wantRevision++
			}
			if got := serverRevision(t, ctx, db); got != wantRevision {
				t.Fatalf("server revision = %d, want %d", got, wantRevision)
			}
		})
	}
}

// rewindRetireBaseAdvanceWatch returns a store at head to the shape a daemon
// at 0127 holds. 0128 changes no schema, so forgetting it is the whole
// rewind; the rows it deleted are not restored, and no rewound fixture needs
// them.
func rewindRetireBaseAdvanceWatch(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE name = '0128_retire_base_advance_watch.sql'`); err != nil {
		t.Fatal(err)
	}
}

// scheduleRowText renders one schedules row's columns for byte comparison.
func scheduleRowText(t *testing.T, ctx context.Context, db *sql.DB, id string) string {
	t.Helper()
	var row string
	if err := db.QueryRowContext(ctx, `SELECT json_array(id, project_id, kind, status,
		generation, run_id, policy_digest, fire_at, entity_version, as_of_revision, body)
		FROM schedules WHERE id = ?`, id).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}
