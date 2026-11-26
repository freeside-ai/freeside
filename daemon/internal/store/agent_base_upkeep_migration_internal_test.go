package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindAdmittedAgentBases returns a migrated store to the schema 0153
// leaves: it rewinds 0155, then drops the two tables 0154 adds and forgets
// 0154.
func rewindAdmittedAgentBases(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindAgentBaseBumpSchedule(t, ctx, db)
	execAll(t, ctx, db,
		`DROP TABLE admitted_agent_base_pointers`,
		`DROP TABLE admitted_agent_bases`,
		`DELETE FROM schema_migrations WHERE name = '0154_admitted_agent_bases.sql'`,
	)
}

// rewindAgentBaseBumpSchedule returns a migrated store to the schema 0154
// leaves: it rebuilds schedules back to the baseline's kind CHECK list,
// keeping its rows, and forgets 0155.
func rewindAgentBaseBumpSchedule(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	execAll(t, ctx, db,
		`CREATE TABLE schedules_0154 (
		    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
		    project_id     TEXT NOT NULL CHECK (project_id <> ''),
		    kind           TEXT NOT NULL CHECK (kind IN (
		                       'pr_checks_deadline', 'review_wait_threshold',
		                       'base_advance_watch', 'installation_poll',
		                       'doctor', 'janitor', 'image_upkeep')),
		    status         TEXT NOT NULL CHECK (status IN (
		                       'armed', 'fired', 'resolved', 'expired')),
		    generation     INTEGER NOT NULL CHECK (generation >= 1),
		    run_id         TEXT REFERENCES runs(id),
		    policy_digest  TEXT CHECK (policy_digest IS NULL OR policy_digest <> ''),
		    fire_at        INTEGER,
		    entity_version INTEGER NOT NULL,
		    as_of_revision INTEGER NOT NULL,
		    body           TEXT NOT NULL,
		    CHECK (
		        (kind IN ('pr_checks_deadline', 'review_wait_threshold', 'base_advance_watch'))
		        = (run_id IS NOT NULL AND policy_digest IS NOT NULL)
		    )
		) STRICT`,
		`INSERT INTO schedules_0154 SELECT * FROM schedules`,
		`DROP TABLE schedules`,
		`ALTER TABLE schedules_0154 RENAME TO schedules`,
		`CREATE INDEX schedules_due ON schedules (status, fire_at)`,
		`DELETE FROM schema_migrations WHERE name = '0155_agent_base_bump_schedule.sql'`,
	)
}

func execAll(t *testing.T, ctx context.Context, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestAgentBaseBumpScheduleMigrationKeepsSchedules: schedules armed at 0154
// read back unchanged after 0155's rebuild, and the rebuilt table stores the
// agent_base_bump kind the old CHECK list refused.
func TestAgentBaseBumpScheduleMigrationKeepsSchedules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawThrough(t, "0155_")
	created := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	interval := int64(3600)
	schedule := func(id domain.ScheduleID, kind domain.ScheduleKind) domain.Schedule {
		s, err := domain.NewSchedule(domain.ScheduleInput{
			ID: id, ProjectID: "project-system", Kind: kind,
			Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
			CreatedAt: created, IntervalSeconds: &interval,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	write := func(fn func(*WriteTx) error) {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fn(&WriteTx{InternalTx: InternalTx{ReadTx: ReadTx{tx: tx}}, asOfRevision: 1}); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	armed := []domain.Schedule{
		schedule("schedule-doctor", domain.ScheduleDoctor),
		schedule("schedule-image-upkeep", domain.ScheduleImageUpkeep),
	}
	write(func(tx *WriteTx) error {
		for _, s := range armed {
			if err := tx.PutSchedule(ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)

	bump := schedule("schedule-agent-base-bump", domain.ScheduleAgentBaseBump)
	write(func(tx *WriteTx) error {
		for _, s := range armed {
			got, err := tx.GetSchedule(ctx, s.ID)
			if err != nil {
				return err
			}
			if got.Kind != s.Kind || got.Generation != s.Generation || got.Status != s.Status {
				t.Errorf("kept schedule %s = %+v, want %+v", s.ID, got, s)
			}
		}
		return tx.PutSchedule(ctx, bump)
	})
}
