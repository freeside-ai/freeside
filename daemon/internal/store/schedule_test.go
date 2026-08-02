package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func schedulePtr[T any](v T) *T { return &v }

func deadlineSchedule(t *testing.T) domain.Schedule {
	t.Helper()
	ts := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	runID := domain.RunID("run-1")
	policyDigest := domain.Digest("sha256:policy")
	s, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-pr_checks_deadline-item-1", ProjectID: "project-1",
		Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: schedulePtr(domain.ItemID("item-1")), ItemVersion: schedulePtr(1),
		},
		RunID: &runID, PolicyDigest: &policyDigest,
		CreatedAt: ts, FireAt: schedulePtr(ts.Add(30 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func janitorSchedule(t *testing.T) domain.Schedule {
	t.Helper()
	ts := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	s, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-janitor", ProjectID: "project-system",
		Kind:      domain.ScheduleJanitor,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: ts, IntervalSeconds: schedulePtr(int64(30)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func putSchedule(t *testing.T, s *store.Store, schedule domain.Schedule) {
	t.Helper()
	if err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		ctx := context.Background()
		if schedule.RunID != nil {
			if err := tx.PutRun(ctx, domain.Run{
				ID: *schedule.RunID, ProjectID: schedule.ProjectID,
				SpecDigest: "sha256:spec", PolicyDigest: *schedule.PolicyDigest,
			}); err != nil {
				return err
			}
		}
		return tx.PutSchedule(ctx, schedule)
	}); err != nil {
		t.Fatalf("PutSchedule: %v", err)
	}
}

func TestScheduleRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	schedule := deadlineSchedule(t)
	putSchedule(t, s, schedule)

	var (
		got  domain.Schedule
		snap store.Snapshot
		list []store.Snapshotted[domain.Schedule]
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, snap, err = tx.GetScheduleSnapshot(ctx, schedule.ID)
		if err != nil {
			return err
		}
		list, err = tx.ListSchedules(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.ID != schedule.ID || got.Generation != 1 || *got.FireAt != *schedule.FireAt {
		t.Fatalf("round trip = %+v", got)
	}
	if snap.EntityVersion != 1 || snap.AsOfRevision < 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(list) != 1 || list[0].Value.ID != schedule.ID {
		t.Fatalf("list = %+v", list)
	}
}

func TestPutScheduleEnforcesTransitions(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	schedule := deadlineSchedule(t)
	putSchedule(t, s, schedule)

	// Same-generation drift (a rewritten deadline) is refused as stale.
	drifted := schedule
	drifted.FireAt = schedulePtr(schedule.FireAt.Add(time.Hour))
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutSchedule(ctx, drifted)
	})
	if !errors.Is(err, store.ErrStaleWrite) {
		t.Fatalf("drift = %v", err)
	}

	// Concluding is legal; rewriting the terminal row is immutable-conflict.
	fired, err := schedule.Concluded(
		domain.ScheduleFired, domain.ResolutionDeadlineElapsed, schedule.FireAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	putSchedule(t, s, fired)
	rewritten := fired
	rewritten.Resolution = &domain.ScheduleResolution{
		Reason: domain.ResolutionDeadlineElapsed, RecordedAt: fired.Resolution.RecordedAt.Add(time.Hour),
	}
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutSchedule(ctx, rewritten)
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("terminal rewrite = %v", err)
	}

	// A re-arm advances the generation and bumps the entity version.
	reArmed, err := fired.ReArmed(fired.Subject,
		schedulePtr(fired.FireAt.Add(2*time.Hour)), fired.FireAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	putSchedule(t, s, reArmed)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		got, snap, err := tx.GetScheduleSnapshot(ctx, schedule.ID)
		if err != nil {
			return err
		}
		if got.Generation != 2 || got.Status != domain.ScheduleArmed || snap.EntityVersion != 3 {
			t.Fatalf("re-armed = gen %d status %s version %d",
				got.Generation, got.Status, snap.EntityVersion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestListDueSchedules(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	deadline := deadlineSchedule(t)
	janitor := janitorSchedule(t)
	putSchedule(t, s, deadline)
	putSchedule(t, s, janitor)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.SetScheduleTimer(ctx, janitor.ID, janitor.Generation, janitor.CreatedAt.Add(30*time.Second))
	}); err != nil {
		t.Fatal(err)
	}

	read := func(now time.Time) []store.DueSchedule {
		t.Helper()
		var due []store.DueSchedule
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			due, err = tx.ListDueSchedules(ctx, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return due
	}

	// Before either nominal instant, nothing is due.
	if due := read(deadline.CreatedAt); len(due) != 0 {
		t.Fatalf("early due = %+v", due)
	}
	// The janitor timer comes due first; the one-shot deadline joins later.
	due := read(deadline.CreatedAt.Add(time.Minute))
	if len(due) != 1 || due[0].Schedule.ID != janitor.ID ||
		!due[0].NominalFireAt.Equal(janitor.CreatedAt.Add(30*time.Second)) {
		t.Fatalf("janitor due = %+v", due)
	}
	due = read(deadline.FireAt.Add(time.Second))
	if len(due) != 2 {
		t.Fatalf("both due = %+v", due)
	}

	// A stale-generation timer is not a due fire.
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.SetScheduleTimer(ctx, janitor.ID, janitor.Generation+1, janitor.CreatedAt)
	}); err != nil {
		t.Fatal(err)
	}
	due = read(deadline.CreatedAt.Add(time.Minute))
	if len(due) != 0 {
		t.Fatalf("stale timer due = %+v", due)
	}

	// A deleted timer silences the recurring schedule.
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.DeleteScheduleTimer(ctx, janitor.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := timerFor(ctx, s, janitor.ID); err != nil || ok {
		t.Fatalf("timer after delete: ok=%v err=%v", ok, err)
	}
}

func timerFor(
	ctx context.Context, s *store.Store, id domain.ScheduleID,
) (int64, time.Time, bool, error) {
	var (
		generation int64
		next       time.Time
		ok         bool
	)
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		generation, next, ok, err = tx.GetScheduleTimer(ctx, id)
		return err
	})
	return generation, next, ok, err
}

func TestScheduleOccurrenceLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	schedule := deadlineSchedule(t)
	putSchedule(t, s, schedule)

	occ := domain.ScheduleOccurrence{
		ScheduleID: schedule.ID, Generation: schedule.Generation,
		NominalFireAt: *schedule.FireAt, Status: domain.OccurrencePending,
		CreatedAt: schedule.FireAt.Add(time.Second),
		Gap: &domain.ScheduleFireGap{
			MissedOccurrences: 2, EarliestMissedAt: schedule.FireAt.Add(-time.Hour),
		},
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		created, err := tx.CreatePendingScheduleOccurrence(ctx, occ)
		if err != nil {
			return err
		}
		if !created {
			t.Error("first create reported no insert")
		}
		// Identity convergence: a crash-retried fire inserts nothing.
		created, err = tx.CreatePendingScheduleOccurrence(ctx, occ)
		if err != nil {
			return err
		}
		if created {
			t.Error("replayed create inserted a second row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var pending []domain.ScheduleOccurrence
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingScheduleOccurrences(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Gap == nil || pending[0].Gap.MissedOccurrences != 2 ||
		!pending[0].NominalFireAt.Equal(*schedule.FireAt) {
		t.Fatalf("pending = %+v", pending)
	}

	// Consumption is guarded on pending status: the first consume wins, a
	// replay reports false, and the row leaves the redelivery scan.
	consumedAt := schedule.FireAt.Add(2 * time.Second)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		consumed, err := tx.ConsumeScheduleOccurrence(ctx,
			schedule.ID, schedule.Generation, *schedule.FireAt, domain.OutcomeHandled, consumedAt)
		if err != nil {
			return err
		}
		if !consumed {
			t.Error("first consume reported no row")
		}
		consumed, err = tx.ConsumeScheduleOccurrence(ctx,
			schedule.ID, schedule.Generation, *schedule.FireAt, domain.OutcomeHandled, consumedAt)
		if err != nil {
			return err
		}
		if consumed {
			t.Error("replayed consume affected a row")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingScheduleOccurrences(ctx)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Fatalf("pending after consume = %+v", pending)
		}
		got, err := tx.GetScheduleOccurrence(ctx, schedule.ID, schedule.Generation, *schedule.FireAt)
		if err != nil {
			return err
		}
		if got.Status != domain.OccurrenceConsumed || got.Outcome == nil ||
			*got.Outcome != domain.OutcomeHandled || !got.ConsumedAt.Equal(consumedAt) {
			t.Fatalf("consumed = %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestScheduleConsumptionAtomicity is the §5.16 transactional bullet at the
// store boundary: a rolled-back consuming transaction leaves the occurrence
// pending and the schedule armed, so the fire is redelivered.
func TestScheduleConsumptionAtomicity(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	schedule := deadlineSchedule(t)
	putSchedule(t, s, schedule)
	occ := domain.ScheduleOccurrence{
		ScheduleID: schedule.ID, Generation: schedule.Generation,
		NominalFireAt: *schedule.FireAt, Status: domain.OccurrencePending,
		CreatedAt: schedule.FireAt.Add(time.Second),
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.CreatePendingScheduleOccurrence(ctx, occ)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("handler outcome failed")
	fired, err := schedule.Concluded(
		domain.ScheduleFired, domain.ResolutionDeadlineElapsed, schedule.FireAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		consumed, err := tx.ConsumeScheduleOccurrence(ctx,
			schedule.ID, schedule.Generation, *schedule.FireAt,
			domain.OutcomeHandled, schedule.FireAt.Add(time.Minute))
		if err != nil {
			return err
		}
		if !consumed {
			t.Error("consume reported no row")
		}
		if err := tx.PutSchedule(ctx, fired); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("write = %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingScheduleOccurrences(ctx)
		if err != nil {
			return err
		}
		if len(pending) != 1 {
			t.Fatalf("pending after rollback = %+v", pending)
		}
		got, err := tx.GetSchedule(ctx, schedule.ID)
		if err != nil {
			return err
		}
		if got.Status != domain.ScheduleArmed {
			t.Fatalf("schedule after rollback = %s", got.Status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
