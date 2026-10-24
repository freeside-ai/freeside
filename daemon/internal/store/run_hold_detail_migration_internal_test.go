package store

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestRunHoldDetailMigrationKeepsOlderHolds: a hold recorded before 0103
// reads back after it with no detail, which the contract allows, and the
// read fails closed on detail columns the contract cannot express.
func TestRunHoldDetailMigrationKeepsOlderHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/run-hold-detail.db", Options{})

	// Return the store to the shape a daemon at 0102 holds, forgetting 0104
	// and then 0103 so the recorded history stays contiguous.
	for _, statement := range []string{
		`ALTER TABLE execution_outcomes DROP COLUMN cause`,
		`DELETE FROM schema_migrations WHERE name = '0104_execution_outcome_cause.sql'`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_check`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_agent`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_launch_stage`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_launch_digest`,
		`DELETE FROM schema_migrations WHERE name = '0103_run_hold_admission_detail.sql'`,
		`INSERT INTO run_hold_observations
			(run_id, invocation_id, reason, first_observed_at, last_observed_at)
			VALUES ('run-1', 'inv-1', 'admission_policy_refused',
				'2026-01-02T03:04:05Z', '2026-01-02T03:05:05Z')`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)

	getHold := func() (domain.RunHoldObservation, error) {
		var (
			hold  domain.RunHoldObservation
			found bool
		)
		err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			hold, found, err = tx.GetRunHold(ctx, "run-1")
			return err
		})
		if err == nil && !found {
			t.Fatal("hold missing after migration")
		}
		return hold, err
	}
	hold, err := getHold()
	if err != nil {
		t.Fatal(err)
	}
	if hold.Reason != domain.HoldAdmissionPolicyRefused || hold.Detail != nil {
		t.Fatalf("older hold = %+v, want admission_policy_refused with no detail", hold)
	}

	for _, tc := range []struct {
		name, update string
		want         error
	}{
		{"unknown check", `UPDATE run_hold_observations SET detail_check = 'vibes'`, domain.ErrInvalidAdmissionCheck},
		{"agent without a check", `UPDATE run_hold_observations
			SET detail_check = NULL, detail_agent = 'implementer-agent'`, domain.ErrHoldDetailMismatch},
		{"launch stage without its digest", `UPDATE run_hold_observations
			SET detail_check = 'agent_launch_coverage', detail_agent = NULL,
				detail_launch_stage = 'specification'`, domain.ErrHoldDetailMismatch},
	} {
		if _, err := st.db.ExecContext(ctx, tc.update); err != nil {
			t.Fatal(err)
		}
		if _, err := getHold(); !errors.Is(err, tc.want) {
			t.Errorf("%s: GetRunHold() = %v, want %v", tc.name, err, tc.want)
		}
	}
}
