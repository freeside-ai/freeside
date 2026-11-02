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

// rewindRunHoldQuotaWindow returns a store at head to the shape a daemon at
// 0118 holds.
func rewindRunHoldQuotaWindow(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE run_hold_observations DROP COLUMN quota_pool`,
		`ALTER TABLE run_hold_observations DROP COLUMN quota_window`,
		`ALTER TABLE run_hold_observations DROP COLUMN quota_source`,
		`ALTER TABLE run_hold_observations DROP COLUMN quota_invocation_id`,
		`ALTER TABLE run_hold_observations DROP COLUMN quota_reading_observed_at`,
		`ALTER TABLE run_hold_observations DROP COLUMN quota_resets_at`,
		`ALTER TABLE run_hold_observations DROP COLUMN quota_set_at`,
		`DELETE FROM schema_migrations WHERE name = '0119_run_hold_quota_window.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunHoldQuotaWindowMigration: a pool hold recorded before 0119 reads
// back unchanged after it, a quota window hold round-trips, and the read
// fails closed on quota columns the contract cannot express.
func TestRunHoldQuotaWindowMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/run-hold-quota-window.db", Options{})
	rewindRunHoldQuotaWindow(t, ctx, st.db)
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

	getHold := func(runID domain.RunID) (domain.RunHoldObservation, error) {
		var hold domain.RunHoldObservation
		err := st.Read(ctx, func(tx *ReadTx) error {
			var (
				found bool
				err   error
			)
			hold, found, err = tx.GetRunHold(ctx, runID)
			if err == nil && !found {
				t.Fatalf("hold on %s missing", runID)
			}
			return err
		})
		return hold, err
	}
	older, err := getHold("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if older.Reason != domain.HoldUsagePoolExhausted || older.UsagePool == nil || older.QuotaWindow != nil {
		t.Fatalf("older hold = %+v, want usage_pool_exhausted on its pool with no quota window", older)
	}

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, mark := range []domain.ExhaustedMark{
		{
			Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal,
			InvocationID: new(domain.InvocationID("inv-refused")), ResetsAt: new(at.Add(5 * time.Hour)), SetAt: at,
		},
		{
			Window: "five_hour", Source: domain.ExhaustedMarkUsageReading,
			ReadingObservedAt: new(at.Add(time.Minute)), ResetsAt: new(at.Add(4 * time.Hour)), SetAt: at.Add(time.Minute),
		},
	} {
		want := domain.RunHoldObservation{
			RunID: "run-2", InvocationID: new(domain.InvocationID("inv-2")),
			Reason:          domain.HoldQuotaWindowExhausted,
			QuotaWindow:     &domain.QuotaWindowHold{Pool: "pool-claude", ExhaustedMark: mark},
			FirstObservedAt: at, LastObservedAt: at,
		}
		if err := st.Write(ctx, func(tx *WriteTx) error { return tx.RecordRunHold(ctx, want) }); err != nil {
			t.Fatal(err)
		}
		got, err := getHold("run-2")
		if err != nil {
			t.Fatal(err)
		}
		if !got.QuotaWindow.ResetsAt.Equal(*mark.ResetsAt) || got.QuotaWindow.Source != mark.Source ||
			(mark.InvocationID != nil) != (got.QuotaWindow.InvocationID != nil) ||
			(mark.ReadingObservedAt != nil) != (got.QuotaWindow.ReadingObservedAt != nil) ||
			!got.QuotaWindow.SetAt.Equal(mark.SetAt) || got.QuotaWindow.Pool != "pool-claude" {
			t.Fatalf("round trip = %+v, want %+v", got.QuotaWindow, want.QuotaWindow)
		}
	}

	for _, tc := range []struct {
		name, update string
		want         error
	}{
		{
			"window without a pool", `UPDATE run_hold_observations SET quota_pool = NULL WHERE run_id = 'run-2'`,
			domain.ErrHoldDetailMismatch,
		},
		{
			"no reset", `UPDATE run_hold_observations SET quota_resets_at = NULL WHERE run_id = 'run-2'`,
			domain.ErrMissingTimestamp,
		},
		{
			"beside another reason", `UPDATE run_hold_observations SET reason = 'operation_stopped' WHERE run_id = 'run-2'`,
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
