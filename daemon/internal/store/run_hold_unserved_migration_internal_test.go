package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// rewindRunHoldUnservedDetail returns a migrated store to the schema 0133
// leaves: no unserved-part columns on run_hold_observations.
func rewindRunHoldUnservedDetail(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindUpdateDrain(t, ctx, db)
	for _, statement := range []string{
		`ALTER TABLE run_hold_observations DROP COLUMN detail_unserved_part`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_unserved_enrollment`,
		`DELETE FROM schema_migrations WHERE name = '0134_run_hold_unserved_detail.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestRunHoldUnservedMigrationKeepsOlderHolds: a hold recorded before 0134
// reads back after it with no unserved part, and the read fails closed on
// unserved columns the contract cannot express (issue #2992).
func TestRunHoldUnservedMigrationKeepsOlderHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	copySnapshotThrough(t, path, "0134_")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO run_hold_observations
		(run_id, invocation_id, reason, detail_check, first_observed_at, last_observed_at)
		VALUES ('run-1', 'inv-1', 'admission_policy_refused', 'agent_resolution',
			'2026-01-02T03:04:05Z', '2026-01-02T03:05:05Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening the store runs 0134 and every migration after it.
	st := openTemplateStoreAt(t, path, Options{})
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
	if hold.Detail == nil || hold.Detail.Check != domain.AdmissionCheckAgentResolution || hold.Detail.Unserved != nil {
		t.Fatalf("older hold detail = %+v, want agent_resolution with no unserved part", hold.Detail)
	}

	for _, tc := range []struct {
		name, update string
		want         error
	}{
		{
			"unknown part",
			`UPDATE run_hold_observations SET detail_unserved_part = 'vibes'`,
			domain.ErrInvalidAgentSelectionPart,
		},
		{
			"model part without its enrollment",
			`UPDATE run_hold_observations SET detail_unserved_part = 'model'`,
			domain.ErrHoldDetailMismatch,
		},
		{
			"part beside another check",
			`UPDATE run_hold_observations SET detail_check = 'agent_prompt', detail_unserved_part = 'enrollment'`,
			domain.ErrHoldDetailMismatch,
		},
		{
			"enrollment without its part",
			`UPDATE run_hold_observations SET detail_check = 'agent_resolution', detail_unserved_part = NULL, detail_unserved_enrollment = 'claude-ig'`,
			domain.ErrHoldDetailMismatch,
		},
		{
			"part without a check",
			`UPDATE run_hold_observations SET detail_check = NULL, detail_unserved_part = 'enrollment', detail_unserved_enrollment = NULL`,
			domain.ErrHoldDetailMismatch,
		},
		{
			"enrollment without a check",
			`UPDATE run_hold_observations SET detail_unserved_part = NULL, detail_unserved_enrollment = 'claude-ig'`,
			domain.ErrHoldDetailMismatch,
		},
	} {
		if _, err := st.db.ExecContext(ctx, tc.update); err != nil {
			t.Fatal(err)
		}
		if _, err := getHold(); !errors.Is(err, tc.want) {
			t.Errorf("%s: GetRunHold() = %v, want %v", tc.name, err, tc.want)
		}
	}
}
