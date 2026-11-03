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

// rewindLaunchReservations returns a store at head to the shape a daemon at
// 0119 holds.
func rewindLaunchReservations(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE launch_waits`,
		`DROP TABLE launch_reservations`,
		`ALTER TABLE run_hold_observations DROP COLUMN capacity_reservation_mib`,
		`ALTER TABLE run_hold_observations DROP COLUMN capacity_reserved_mib`,
		`ALTER TABLE run_hold_observations DROP COLUMN capacity_budget_mib`,
		`ALTER TABLE run_hold_observations DROP COLUMN capacity_ahead`,
		`DELETE FROM schema_migrations WHERE name = '0120_launch_reservations.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLaunchReservationsMigration: a hold recorded before 0120 reads back
// unchanged after it, a machine capacity hold round-trips, and the read fails
// closed on capacity columns the contract cannot express.
func TestLaunchReservationsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/launch-reservations.db", Options{})
	rewindLaunchReservations(t, ctx, st.db)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO run_hold_observations
		(run_id, invocation_id, reason, usage_pool, first_observed_at, last_observed_at)
		VALUES ('run-1', 'inv-1', 'usage_pool_exhausted', 'pool-claude',
			'2026-01-02T03:04:05Z', '2026-01-02T03:05:05Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)

	getHold := func(runID domain.RunID) domain.RunHoldObservation {
		t.Helper()
		var hold domain.RunHoldObservation
		if err := st.Read(ctx, func(tx *ReadTx) error {
			var (
				found bool
				err   error
			)
			hold, found, err = tx.GetRunHold(ctx, runID)
			if err == nil && !found {
				err = errors.New("hold missing")
			}
			return err
		}); err != nil {
			t.Fatalf("hold on %s: %v", runID, err)
		}
		return hold
	}
	if older := getHold("run-1"); older.Reason != domain.HoldUsagePoolExhausted || older.MachineCapacity != nil {
		t.Fatalf("older hold = %+v, want usage_pool_exhausted with no machine capacity", older)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fact := domain.MachineCapacityHold{ReservationMiB: 2304, ReservedMiB: 2304, BudgetMiB: 4096, Ahead: 1}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: "run-2", InvocationID: new(domain.InvocationID("inv-2")),
			Reason: domain.HoldMachineCapacity, MachineCapacity: &fact,
			FirstObservedAt: at, LastObservedAt: at,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if got := getHold("run-2"); got.MachineCapacity == nil || *got.MachineCapacity != fact {
		t.Fatalf("round trip = %+v, want %+v", got.MachineCapacity, fact)
	}

	for _, tc := range []struct {
		name, update string
		want         error
	}{
		{
			"partial columns", `UPDATE run_hold_observations SET capacity_budget_mib = NULL WHERE run_id = 'run-2'`,
			domain.ErrHoldDetailMismatch,
		},
		{
			"beside another reason", `UPDATE run_hold_observations SET reason = 'operation_stopped' WHERE run_id = 'run-2'`,
			domain.ErrHoldDetailMismatch,
		},
		{
			"fits with none ahead", `UPDATE run_hold_observations SET capacity_ahead = 0, capacity_reserved_mib = 0 WHERE run_id = 'run-2'`,
			domain.ErrHoldDetailMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := st.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(ctx, tc.update); err != nil {
				t.Fatal(err)
			}
			readTx := &ReadTx{tx: tx}
			if _, _, err := readTx.GetRunHold(ctx, "run-2"); !errors.Is(err, tc.want) {
				t.Fatalf("GetRunHold = %v, want %v", err, tc.want)
			}
		})
	}
}
