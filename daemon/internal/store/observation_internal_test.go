package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// seedObservationStore opens a bare store for tamper tests; the observation
// tables have no foreign keys, so no fixture rows are needed.
func seedObservationStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir()+"/observation.db", Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestObservationReadsFailClosedOnTamper: a row the current vocabulary
// cannot express is a read error, never a silently thinner observation. The
// raw inserts bypass the write boundary the way tampering or a future schema
// would.
func TestObservationReadsFailClosedOnTamper(t *testing.T) {
	ctx := context.Background()
	at := formatTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	observe := func(s *Store) error {
		return s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ObserveRun(ctx, "run-1")
			return err
		})
	}

	t.Run("milestone detail outside its kind", func(t *testing.T) {
		s := seedObservationStore(t)
		// The kind CHECK constrains kind itself, so tamper the detail
		// columns: a submitted milestone carrying a reason.
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO run_milestones
			   (run_id, kind, invocation_id, reason, recorded_at)
			 VALUES ('run-1', 'run_submitted', 'inv-1', 'trust_blocked', ?)`, at); err != nil {
			t.Fatalf("seed tampered milestone: %v", err)
		}
		if err := observe(s); !errors.Is(err, domain.ErrMilestoneDetailMismatch) {
			t.Errorf("ObserveRun = %v, want %v", err, domain.ErrMilestoneDetailMismatch)
		}
	})

	t.Run("milestone with unknown reason code", func(t *testing.T) {
		s := seedObservationStore(t)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO run_milestones
			   (run_id, kind, invocation_id, reason, recorded_at)
			 VALUES ('run-1', 'publication_blocked', 'inv-1', 'made_up_reason', ?)`, at); err != nil {
			t.Fatalf("seed tampered milestone: %v", err)
		}
		if err := observe(s); !errors.Is(err, domain.ErrInvalidRunHoldReason) {
			t.Errorf("ObserveRun = %v, want %v", err, domain.ErrInvalidRunHoldReason)
		}
	})

	t.Run("observation with unknown status", func(t *testing.T) {
		s := seedObservationStore(t)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO invocation_observations
			   (invocation_id, run_id, status, live, observed_at)
			 VALUES ('inv-1', 'run-1', 'paused', 0, ?)`, at); err != nil {
			t.Fatalf("seed tampered observation: %v", err)
		}
		if err := observe(s); !errors.Is(err, domain.ErrInvalidObservedStatus) {
			t.Errorf("ObserveRun = %v, want %v", err, domain.ErrInvalidObservedStatus)
		}
	})

	t.Run("hold with unknown reason", func(t *testing.T) {
		s := seedObservationStore(t)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO run_hold_observations
			   (run_id, invocation_id, reason, first_observed_at, last_observed_at)
			 VALUES ('run-1', 'inv-1', 'vibes', ?, ?)`, at, at); err != nil {
			t.Fatalf("seed tampered hold: %v", err)
		}
		if err := observe(s); !errors.Is(err, domain.ErrInvalidRunHoldReason) {
			t.Errorf("ObserveRun = %v, want %v", err, domain.ErrInvalidRunHoldReason)
		}
	})

	t.Run("hold write repairs a corrupt row instead of failing", func(t *testing.T) {
		s := seedObservationStore(t)
		// A row the vocabulary cannot express and whose instant does not
		// parse: the read surface fails closed on it (above), but the write
		// path must repair it by overwrite — trusting it enough to error
		// would hand a forged projection row the power to fail the workflow
		// pass carrying the write.
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO run_hold_observations
			   (run_id, invocation_id, reason, first_observed_at, last_observed_at)
			 VALUES ('run-1', 'inv-1', 'vibes', 'yesterday', 'today')`); err != nil {
			t.Fatalf("seed corrupt hold: %v", err)
		}
		now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		inv := domain.InvocationID("inv-1")
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.RecordRunHold(ctx, domain.RunHoldObservation{
				RunID: "run-1", InvocationID: &inv,
				Reason:          domain.HoldOperationStopped,
				FirstObservedAt: now, LastObservedAt: now,
			})
		}); err != nil {
			t.Fatalf("RecordRunHold over a corrupt row = %v, want repair", err)
		}
		if err := s.Read(ctx, func(tx *ReadTx) error {
			hold, found, err := tx.GetRunHold(ctx, "run-1")
			if err != nil || !found || hold.Reason != domain.HoldOperationStopped {
				t.Errorf("repaired hold = %+v, %v, %v", hold, found, err)
			}
			return nil
		}); err != nil {
			t.Fatalf("read repaired hold: %v", err)
		}
	})

	t.Run("milestone with unparsable instant", func(t *testing.T) {
		s := seedObservationStore(t)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO run_milestones
			   (run_id, kind, invocation_id, recorded_at)
			 VALUES ('run-1', 'run_submitted', 'inv-1', 'yesterday')`); err != nil {
			t.Fatalf("seed tampered milestone: %v", err)
		}
		if err := observe(s); err == nil {
			t.Error("ObserveRun accepted an unparsable instant")
		}
	})
}

// TestAuthorityReplayDoesNotBackfillMilestones pins migration 0024's
// no-backfill rule at the execution-record hooks: a byte-identical replay of
// an admission, export, or outcome recorded before the migration (simulated
// by erasing the milestones the insert wrote) must not mint milestones the
// run never had observed.
func TestAuthorityReplayDoesNotBackfillMilestones(t *testing.T) {
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)

	outcome := domain.ExecutionOutcome{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: "failed once",
		RecordedAt: admission.AdmittedAt.Add(time.Hour),
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordExecutionOutcome(ctx, outcome)
	}); err != nil {
		t.Fatalf("record outcome: %v", err)
	}

	countMilestones := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM run_milestones WHERE run_id = ?`,
			admission.RunID).Scan(&n); err != nil {
			t.Fatalf("count milestones: %v", err)
		}
		return n
	}
	if countMilestones() == 0 {
		t.Fatal("the inserting writes recorded no milestones to erase")
	}
	// Simulate the pre-0024 shape: the authorities exist, the timeline does
	// not.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM run_milestones WHERE run_id = ?`, admission.RunID); err != nil {
		t.Fatalf("erase milestones: %v", err)
	}

	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		return tx.RecordExecutionOutcome(ctx, outcome)
	}); err != nil {
		t.Fatalf("replay records: %v", err)
	}
	if n := countMilestones(); n != 0 {
		t.Errorf("replayed authorities backfilled %d milestone(s)", n)
	}

	// The export hook shares the rule; its authority excludes the outcome,
	// so exercise it on a fresh store.
	s2, admission2 := seedAdmission(t, nil)
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: admission2.InvocationID, AdmissionID: admission2.ID,
		ObservedBaseSHA: admission2.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest",
		RecordedAt:     admission2.AdmittedAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	if err := s2.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordExecutionExport(ctx, export)
	}); err != nil {
		t.Fatalf("record export: %v", err)
	}
	if _, err := s2.db.ExecContext(ctx,
		`DELETE FROM run_milestones WHERE run_id = ?`, admission2.RunID); err != nil {
		t.Fatalf("erase milestones: %v", err)
	}
	if err := s2.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordExecutionExport(ctx, export)
	}); err != nil {
		t.Fatalf("replay export: %v", err)
	}
	var n int
	if err := s2.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_milestones WHERE run_id = ?`,
		admission2.RunID).Scan(&n); err != nil {
		t.Fatalf("count milestones: %v", err)
	}
	if n != 0 {
		t.Errorf("replayed export backfilled %d milestone(s)", n)
	}
}
