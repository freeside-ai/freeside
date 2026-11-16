package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindDaemonReservations returns a store at head to the shape a daemon at
// 0143 holds: both tables rebuilt back to a required run and the three
// launch kinds, keeping their rows.
func rewindDaemonReservations(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindProjectReadiness(t, ctx, db)
	rewindRunHoldSpans(t, ctx, db)
	for _, statement := range []string{
		`CREATE TABLE launch_reservations_0143 (
		    invocation_id TEXT    NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
		    run_id        TEXT    NOT NULL CHECK (run_id <> ''),
		    kind          TEXT    NOT NULL CHECK (kind IN ('execution', 'review', 'verification')),
		    memory_mib    INTEGER NOT NULL CHECK (memory_mib > 0),
		    overhead_mib  INTEGER NOT NULL CHECK (overhead_mib >= 0),
		    reserved_at   TEXT    NOT NULL CHECK (reserved_at <> ''),
		    released_at   TEXT    CHECK (released_at IS NULL OR released_at <> '')
		) STRICT`,
		`INSERT INTO launch_reservations_0143 SELECT * FROM launch_reservations`,
		`DROP TABLE launch_reservations`,
		`ALTER TABLE launch_reservations_0143 RENAME TO launch_reservations`,
		`CREATE INDEX launch_reservations_open ON launch_reservations (released_at)`,
		`CREATE TABLE launch_waits_0143 (
		    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
		    run_id          TEXT NOT NULL CHECK (run_id <> ''),
		    task_created_at TEXT NOT NULL CHECK (task_created_at <> ''),
		    waiting_since   TEXT NOT NULL CHECK (waiting_since <> ''),
		    refused_at      TEXT NOT NULL CHECK (refused_at <> ''),
		    kept_at         TEXT CHECK (kept_at IS NULL OR kept_at <> '')
		) STRICT`,
		`INSERT INTO launch_waits_0143 SELECT * FROM launch_waits`,
		`DROP TABLE launch_waits`,
		`ALTER TABLE launch_waits_0143 RENAME TO launch_waits`,
		`DELETE FROM schema_migrations WHERE name = '0144_daemon_reservations.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestDaemonReservationsMigrationKeepsRows: an open reservation, a released
// one, and a live wait recorded at 0143 read back unchanged after 0144, and
// the rebuilt tables then hold a reservation and a wait with no run.
func TestDaemonReservationsMigrationKeepsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawThrough(t, "0144_")
	for _, statement := range []string{
		`INSERT INTO launch_reservations
			(invocation_id, run_id, kind, memory_mib, overhead_mib, reserved_at, released_at)
			VALUES ('inv-open', 'run-1', 'verification', 2048, 256, '2026-01-02T03:04:05Z', NULL),
			       ('inv-released', 'run-2', 'execution', 4096, 256, '2026-01-02T03:04:05Z', '2026-01-02T03:09:05Z')`,
		`INSERT INTO launch_waits (invocation_id, run_id, task_created_at, waiting_since, refused_at, kept_at)
			VALUES ('inv-wait', 'run-3', '2026-01-02T03:00:00Z', '2026-01-02T03:05:05Z',
				'2026-01-02T03:06:05Z', '2026-01-02T03:07:05Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	read := &ReadTx{tx: tx}
	open, found, err := read.getLaunchReservation(ctx, "inv-open")
	wantOpen := domain.LaunchReservation{
		InvocationID: "inv-open", RunID: "run-1", Kind: domain.ReservationVerification, MemoryMiB: 2048, OverheadMiB: 256,
	}
	if err != nil || !found || open != wantOpen {
		t.Fatalf("open reservation = %+v, %t, %v; want %+v", open, found, err, wantOpen)
	}
	listed, err := read.openLaunchReservations(ctx)
	if err != nil || len(listed) != 1 || listed[0] != wantOpen {
		t.Fatalf("open reservations = %+v, %v; want only the open one", listed, err)
	}
	var releasedAt string
	if err := tx.QueryRowContext(ctx,
		`SELECT released_at FROM launch_reservations WHERE invocation_id = 'inv-released'`).Scan(&releasedAt); err != nil ||
		releasedAt != "2026-01-02T03:09:05Z" {
		t.Fatalf("released reservation released_at = %q, %v", releasedAt, err)
	}
	waits, err := read.listLaunchWaits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	want := launchWait{
		invocation: "inv-wait", run: "run-3", taskCreatedAt: at("2026-01-02T03:00:00Z"),
		waitingSince: at("2026-01-02T03:05:05Z"), refusedAt: at("2026-01-02T03:06:05Z"), keptAt: at("2026-01-02T03:07:05Z"),
	}
	if len(waits) != 1 || waits[0] != want {
		t.Fatalf("waits = %+v, want %+v", waits, want)
	}

	for _, statement := range []string{
		`INSERT INTO launch_reservations (invocation_id, run_id, kind, memory_mib, overhead_mib, reserved_at)
			VALUES ('proof-pass-1', NULL, 'proof_pass', 2048, 512, '2026-01-02T03:04:05Z')`,
		`INSERT INTO launch_waits (invocation_id, run_id, task_created_at, waiting_since, refused_at)
			VALUES ('proof-pass-2', NULL, NULL, '2026-01-02T03:05:05Z', '2026-01-02T03:05:05Z')`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	// A wait names its run and its task's submission together, or neither.
	if _, err := tx.ExecContext(ctx, `INSERT INTO launch_waits
		(invocation_id, run_id, task_created_at, waiting_since, refused_at)
		VALUES ('inv-half', 'run-4', NULL, '2026-01-02T03:05:05Z', '2026-01-02T03:05:05Z')`); err == nil {
		t.Fatal("a wait with a run and no task submission was accepted")
	}
	daemon, found, err := read.getLaunchReservation(ctx, "proof-pass-1")
	if err != nil || !found || daemon.RunID != "" || daemon.Kind != domain.ReservationProofPass {
		t.Fatalf("daemon reservation = %+v, %t, %v", daemon, found, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE launch_reservations SET kind = 'upkeep'
		WHERE invocation_id = 'proof-pass-1'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := read.getLaunchReservation(ctx, "proof-pass-1"); !errors.Is(err, domain.ErrInvalidLaunchReservationKind) {
		t.Fatalf("unknown stored kind = %v, want %v", err, domain.ErrInvalidLaunchReservationKind)
	}
}
