package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// causelessFailedBody is a failed outcome's body as it was stored before
// causes existed: omitempty leaves the field out entirely.
func causelessFailedBody(t *testing.T, admission domain.ExecutionAdmission) string {
	t.Helper()
	body, err := json.Marshal(domain.ExecutionOutcome{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: "failed before causes",
		RecordedAt: admission.AdmittedAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func insertRawOutcome(
	t *testing.T, ctx context.Context, s *Store, admission domain.ExecutionAdmission, cause, body string,
) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO execution_outcomes
		   (invocation_id, admission_id, status, summary, cause, recorded_at, body)
		 VALUES (?, ?, 'failed', 'failed before causes', ?, ?, ?)`,
		admission.InvocationID, admission.ID, cause,
		formatTime(admission.AdmittedAt.Add(time.Hour)), body); err != nil {
		t.Fatalf("insert raw outcome: %v", err)
	}
}

// TestExecutionOutcomeReadsCauselessFailedRow covers a failed row written
// before causes existed, as migration 0104 leaves it: the body has no cause
// and the column says unknown, and the row reads back with cause unknown.
func TestExecutionOutcomeReadsCauselessFailedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)
	insertRawOutcome(t, ctx, s, admission, "unknown", causelessFailedBody(t, admission))

	if err := s.Read(ctx, func(tx *ReadTx) error {
		for name, get := range map[string]func(context.Context, domain.InvocationID) (domain.ExecutionOutcome, error){
			"current": tx.GetExecutionOutcome, "record": tx.GetExecutionOutcomeRecord,
		} {
			outcome, err := get(ctx, admission.InvocationID)
			if err != nil {
				return err
			}
			if outcome.Cause != domain.FailureCauseUnknown {
				t.Errorf("%s read cause = %q, want unknown", name, outcome.Cause)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read causeless failed row: %v", err)
	}
}

// TestExecutionOutcomeCauseColumnIsCrossChecked refuses a row whose cause
// column disagrees with its body, as status and summary already are.
func TestExecutionOutcomeCauseColumnIsCrossChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)
	insertRawOutcome(t, ctx, s, admission, "quota", causelessFailedBody(t, admission))

	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetExecutionOutcomeRecord(ctx, admission.InvocationID)
		return err
	})
	if !errors.Is(err, ErrRowInconsistent) {
		t.Fatalf("read with a disagreeing cause column = %v, want ErrRowInconsistent", err)
	}
}

// TestExecutionOutcomeCauseRoundTrip records a failed outcome with a positive
// cause and reads it back unchanged.
func TestExecutionOutcomeCauseRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)
	want := domain.ExecutionOutcome{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: "quota exhausted",
		Cause: domain.FailureCauseQuota, RecordedAt: admission.AdmittedAt.Add(time.Hour),
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, want)
	}); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	var got domain.ExecutionOutcome
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetExecutionOutcomeRecord(ctx, admission.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// TestExecutionOutcomeCauseMigration is the migration acceptance for 0104: a
// failed row written before it carries unknown afterwards, and a canceled row
// carries no cause.
func TestExecutionOutcomeCauseMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)
	seedRawAdmission(t, ctx, s, admission)
	rewindUsagePools(t, ctx, s.db)
	for _, statement := range []string{
		`ALTER TABLE task_lines DROP COLUMN provenance`,
		`DELETE FROM schema_migrations WHERE name = '0111_task_line_provenance.sql'`,
		`DROP TABLE image_policy_adoptions`,
		`DELETE FROM schema_migrations WHERE name = '0110_image_policy_adoption.sql'`,
		`DELETE FROM schema_migrations WHERE name = '0109_task_submission_origin.sql'`,
		`DROP TABLE repository_tip_observations`,
		`DROP TABLE image_upkeep_refusals`,
		`DELETE FROM schema_migrations WHERE name = '0108_image_upkeep.sql'`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_pending`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_refusal_id`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_item_id`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_task_id`,
		`DELETE FROM schema_migrations WHERE name = '0107_run_hold_image_upkeep.sql'`,
		`DELETE FROM schema_migrations WHERE name = '0106_schedules_image_upkeep.sql'`,
		`DROP TABLE admitted_image_pointers`,
		`DELETE FROM schema_migrations WHERE name = '0105_admitted_image_pointers.sql'`,
		`ALTER TABLE execution_outcomes DROP COLUMN cause`,
		`DELETE FROM schema_migrations WHERE name = '0104_execution_outcome_cause.sql'`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	recordedAt := formatTime(admission.AdmittedAt.Add(time.Hour))
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO execution_outcomes
		   (invocation_id, admission_id, status, summary, recorded_at, body)
		 VALUES (?, ?, 'failed', 'failed before causes', ?, ?),
		        ('inv-2', 'sha256:other', 'canceled', 'canceled', ?, '{}')`,
		admission.InvocationID, admission.ID, recordedAt, causelessFailedBody(t, admission),
		recordedAt); err != nil {
		t.Fatalf("seed pre-migration outcomes: %v", err)
	}

	if err := migrate(ctx, s.db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	for invocation, want := range map[string]string{
		string(admission.InvocationID): "unknown", "inv-2": "",
	} {
		var cause string
		if err := s.db.QueryRowContext(ctx,
			`SELECT cause FROM execution_outcomes WHERE invocation_id = ?`, invocation).
			Scan(&cause); err != nil {
			t.Fatalf("read %s cause: %v", invocation, err)
		}
		if cause != want {
			t.Errorf("%s cause = %q after migration, want %q", invocation, cause, want)
		}
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		outcome, err := tx.GetExecutionOutcomeRecord(ctx, admission.InvocationID)
		if err == nil && outcome.Cause != domain.FailureCauseUnknown {
			t.Errorf("migrated failed outcome cause = %q, want unknown", outcome.Cause)
		}
		return err
	}); err != nil {
		t.Fatalf("read migrated failed outcome: %v", err)
	}
}
