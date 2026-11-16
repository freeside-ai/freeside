package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func recordSpanTestHold(
	t *testing.T, s *store.Store, runID domain.RunID, inv *domain.InvocationID,
	reason domain.RunHoldReason, at time.Time,
) {
	t.Helper()
	if err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.RecordRunHold(context.Background(), domain.RunHoldObservation{
			RunID: runID, InvocationID: inv, Reason: reason,
			FirstObservedAt: at, LastObservedAt: at,
		})
	}); err != nil {
		t.Fatalf("record hold %s: %v", reason, err)
	}
}

func listRunHoldSpans(t *testing.T, s *store.Store, runID domain.RunID) []domain.RunHoldSpan {
	t.Helper()
	var spans []domain.RunHoldSpan
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		spans, err = tx.ListRunHoldSpans(context.Background(), runID)
		return err
	}); err != nil {
		t.Fatalf("list run hold spans %s: %v", runID, err)
	}
	return spans
}

func holdSpan(
	runID domain.RunID, inv *domain.InvocationID, reason domain.RunHoldReason,
	first, last time.Time, cleared *time.Time,
) domain.RunHoldSpan {
	return domain.RunHoldSpan{
		RunID: runID, InvocationID: inv, Reason: reason,
		FirstObservedAt: first, LastObservedAt: last, ClearedAt: cleared,
	}
}

func assertSpans(t *testing.T, what string, got, want []domain.RunHoldSpan) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

// TestRunHoldSpanLifecycle walks one run through every way a hold continues
// or ends and checks the history after each step: a span closes only when
// its cause ends, at the instant the issue contract names for each path.
func TestRunHoldSpanLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	const run = domain.RunID("run-1")
	inv1, inv2 := domain.InvocationID("inv-1"), domain.InvocationID("inv-2")
	at := func(minutes int) time.Time { return admissionEpoch.Add(time.Duration(minutes) * time.Minute) }

	// Recording a hold opens one span.
	recordSpanTestHold(t, s, run, &inv1, domain.HoldOperationStopped, at(0))
	assertSpans(t, "record", listRunHoldSpans(t, s, run), []domain.RunHoldSpan{
		holdSpan(run, &inv1, domain.HoldOperationStopped, at(0), at(0), nil),
	})

	// Re-observing the same cause extends the open span.
	recordSpanTestHold(t, s, run, &inv1, domain.HoldOperationStopped, at(1))
	stopped := holdSpan(run, &inv1, domain.HoldOperationStopped, at(0), at(1), nil)
	assertSpans(t, "same cause", listRunHoldSpans(t, s, run), []domain.RunHoldSpan{stopped})

	// A new reason closes the span at the new hold's first instant.
	recordSpanTestHold(t, s, run, &inv1, domain.HoldBlockingSystemHealth, at(2))
	stopped.ClearedAt = ptrTime(at(2))
	health1 := holdSpan(run, &inv1, domain.HoldBlockingSystemHealth, at(2), at(2), nil)
	assertSpans(t, "new reason", listRunHoldSpans(t, s, run), []domain.RunHoldSpan{stopped, health1})

	// A new invocation alone is a new cause too.
	recordSpanTestHold(t, s, run, &inv2, domain.HoldBlockingSystemHealth, at(3))
	health1.ClearedAt = ptrTime(at(3))
	health2 := holdSpan(run, &inv2, domain.HoldBlockingSystemHealth, at(3), at(3), nil)
	assertSpans(t, "new invocation", listRunHoldSpans(t, s, run),
		[]domain.RunHoldSpan{stopped, health1, health2})

	// A milestone that inserts closes the span at its recorded instant.
	started := domain.RunMilestone{
		RunID: run, Kind: domain.MilestoneInvocationStarted,
		InvocationID: &inv2, RecordedAt: at(4),
	}
	appendMilestone(t, s, started)
	health2.ClearedAt = ptrTime(at(4))
	closed := []domain.RunHoldSpan{stopped, health1, health2}
	assertSpans(t, "milestone", listRunHoldSpans(t, s, run), closed)

	// A replayed milestone clears nothing, so nothing closes.
	recordSpanTestHold(t, s, run, nil, domain.HoldOperationStopped, at(5))
	started.RecordedAt = at(6)
	appendMilestone(t, s, started)
	open := holdSpan(run, nil, domain.HoldOperationStopped, at(5), at(5), nil)
	assertSpans(t, "replayed milestone", listRunHoldSpans(t, s, run), append(closed, open))

	// A clock stepped back restarts the same cause: only its start moves,
	// and no span is written.
	recordSpanTestHold(t, s, run, nil, domain.HoldOperationStopped, at(4))
	open = holdSpan(run, nil, domain.HoldOperationStopped, at(4), at(4), nil)
	assertSpans(t, "stepped-back clock", listRunHoldSpans(t, s, run), append(closed, open))

	clearCause := func(reason domain.RunHoldReason) {
		t.Helper()
		if err := s.Write(ctx, func(tx *store.WriteTx) error {
			return tx.ClearRunHoldCause(ctx, run, reason)
		}); err != nil {
			t.Fatalf("clear cause %s: %v", reason, err)
		}
	}
	// Clearing another cause leaves the hold and the history alone.
	clearCause(domain.HoldAttendedModeActive)
	assertSpans(t, "clear other cause", listRunHoldSpans(t, s, run), append(closed, open))

	// The two clears without an instant close the span on the store's clock.
	assertWallClockClear := func(what string, clear func()) {
		t.Helper()
		before := time.Now().UTC()
		clear()
		after := time.Now().UTC()
		spans := listRunHoldSpans(t, s, run)
		last := spans[len(spans)-1]
		if last.ClearedAt == nil || last.ClearedAt.Before(before) || last.ClearedAt.After(after) {
			t.Fatalf("%s: cleared at %v, want within [%v, %v]", what, last.ClearedAt, before, after)
		}
		open.ClearedAt = last.ClearedAt
		closed = append(closed, open)
		assertSpans(t, what, spans, closed)
	}
	assertWallClockClear("clear matching cause", func() { clearCause(domain.HoldOperationStopped) })

	recordSpanTestHold(t, s, run, &inv1, domain.HoldTrustBlocked, at(7))
	open = holdSpan(run, &inv1, domain.HoldTrustBlocked, at(7), at(7), nil)
	assertWallClockClear("clear run hold", func() {
		if err := s.Write(ctx, func(tx *store.WriteTx) error {
			return tx.ClearRunHold(ctx, run)
		}); err != nil {
			t.Fatalf("clear hold: %v", err)
		}
	})

	// Ending no hold writes nothing.
	clearCause(domain.HoldTrustBlocked)
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.ClearRunHold(ctx, run) }); err != nil {
		t.Fatalf("clear absent hold: %v", err)
	}
	assertSpans(t, "clear with no hold", listRunHoldSpans(t, s, run), closed)
}

// TestRunHoldSpanClearTimeNotBeforeLastObservation: an ending clear time
// earlier than the hold's last observation, from a milestone or from a new
// cause whose clock reads earlier, is stored as the last observation.
func TestRunHoldSpanClearTimeNotBeforeLastObservation(t *testing.T) {
	t.Parallel()
	s := openStore(t, store.Options{})
	const run = domain.RunID("run-1")
	inv := domain.InvocationID("inv-1")
	at := func(minutes int) time.Time { return admissionEpoch.Add(time.Duration(minutes) * time.Minute) }

	recordSpanTestHold(t, s, run, &inv, domain.HoldOperationStopped, at(10))
	recordSpanTestHold(t, s, run, &inv, domain.HoldBlockingSystemHealth, at(9))
	appendMilestone(t, s, domain.RunMilestone{
		RunID: run, Kind: domain.MilestoneInvocationStarted, InvocationID: &inv, RecordedAt: at(5),
	})
	assertSpans(t, "early clears", listRunHoldSpans(t, s, run), []domain.RunHoldSpan{
		holdSpan(run, &inv, domain.HoldOperationStopped, at(10), at(10), ptrTime(at(10))),
		holdSpan(run, &inv, domain.HoldBlockingSystemHealth, at(9), at(9), ptrTime(at(9))),
	})
}

// TestRunHoldSpanSurvivesRestart: a hold open when the store closes is
// still one hold when it is re-observed after the store reopens, and it is
// written once when it ends.
func TestRunHoldSpanSurvivesRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	const run = domain.RunID("run-1")
	inv := domain.InvocationID("inv-1")
	first := openStoreAt(t, path, store.Options{})
	recordSpanTestHold(t, first, run, &inv, domain.HoldOperationStopped, admissionEpoch)
	if err := first.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	s := openStoreAt(t, path, store.Options{})
	recordSpanTestHold(t, s, run, &inv, domain.HoldOperationStopped, admissionEpoch.Add(time.Minute))
	span := holdSpan(run, &inv, domain.HoldOperationStopped, admissionEpoch, admissionEpoch.Add(time.Minute), nil)
	assertSpans(t, "after restart", listRunHoldSpans(t, s, run), []domain.RunHoldSpan{span})

	appendMilestone(t, s, domain.RunMilestone{
		RunID: run, Kind: domain.MilestoneInvocationStarted, InvocationID: &inv,
		RecordedAt: admissionEpoch.Add(2 * time.Minute),
	})
	span.ClearedAt = ptrTime(admissionEpoch.Add(2 * time.Minute))
	assertSpans(t, "ended after restart", listRunHoldSpans(t, s, run), []domain.RunHoldSpan{span})
}

// TestListRunHoldSpansBetween: the range read returns every run's spans that
// overlap [from, to), including one that crosses each end and holds still
// open, ordered by first observation, then run.
func TestListRunHoldSpansBetween(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	at := func(minutes int) time.Time { return admissionEpoch.Add(time.Duration(minutes) * time.Minute) }
	inv := domain.InvocationID("inv-1")
	// heldBetween records a hold of run from first to last and ends it at
	// cleared with a milestone, so every instant is fixed.
	heldBetween := func(run domain.RunID, first, cleared time.Time) domain.RunHoldSpan {
		t.Helper()
		recordSpanTestHold(t, s, run, &inv, domain.HoldOperationStopped, first)
		appendMilestone(t, s, domain.RunMilestone{
			RunID: run, Kind: domain.MilestoneInvocationStarted, InvocationID: &inv, RecordedAt: cleared,
		})
		return holdSpan(run, &inv, domain.HoldOperationStopped, first, first, &cleared)
	}
	heldSince := func(run domain.RunID, first time.Time) domain.RunHoldSpan {
		t.Helper()
		recordSpanTestHold(t, s, run, nil, domain.HoldTrustBlocked, first)
		return holdSpan(run, nil, domain.HoldTrustBlocked, first, first, nil)
	}

	crossesStart := heldBetween("run-a", at(0), at(2))
	openA := heldSince("run-a", at(5))
	inside := heldBetween("run-b", at(3), at(4))
	crossesEnd := heldBetween("run-c", at(10), at(12))
	heldBetween("run-d", at(-2), at(1)) // Cleared at from: no overlap.
	heldSince("run-e", at(11))          // Starts at to: no overlap.
	sameStart := heldSince("run-f", at(3))
	// Stored as text, 03:07:05.5Z sorts before 03:07:05Z; the read orders
	// by instant.
	subSecond := heldSince("run-0", at(3).Add(500*time.Millisecond))

	var got []domain.RunHoldSpan
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.ListRunHoldSpansBetween(ctx, at(1), at(11))
		return err
	}); err != nil {
		t.Fatalf("list run hold spans between: %v", err)
	}
	assertSpans(t, "range", got, []domain.RunHoldSpan{crossesStart, inside, sameStart, subSecond, openA, crossesEnd})

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListRunHoldSpansBetween(ctx, at(1), at(1))
		return err
	}); !errors.Is(err, domain.ErrTimestampOutOfOrder) {
		t.Errorf("empty range: ListRunHoldSpansBetween() = %v, want %v", err, domain.ErrTimestampOutOfOrder)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListRunHoldSpans(ctx, "")
		return err
	}); !errors.Is(err, domain.ErrEmptyID) {
		t.Errorf("empty run: ListRunHoldSpans() = %v, want %v", err, domain.ErrEmptyID)
	}
}

// TestRunHoldSpanUpdateDrainLift: lifting a drain ends every run's
// update_drain hold at once, so each gets a span on the store's clock, and a
// hold of another cause stays open.
func TestRunHoldSpanUpdateDrainLift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	recordSpanTestHold(t, s, "run-a", nil, domain.HoldUpdateDrain, admissionEpoch)
	recordSpanTestHold(t, s, "run-b", nil, domain.HoldUpdateDrain, admissionEpoch.Add(time.Minute))
	recordSpanTestHold(t, s, "run-c", nil, domain.HoldTrustBlocked, admissionEpoch)

	before := time.Now().UTC()
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.ClearUpdateDrainHolds(ctx) }); err != nil {
		t.Fatalf("clear update drain holds: %v", err)
	}
	after := time.Now().UTC()

	for _, run := range []domain.RunID{"run-a", "run-b"} {
		spans := listRunHoldSpans(t, s, run)
		if len(spans) != 1 || spans[0].Reason != domain.HoldUpdateDrain || spans[0].ClearedAt == nil ||
			spans[0].ClearedAt.Before(before) || spans[0].ClearedAt.After(after) {
			t.Errorf("%s spans = %+v, want one update_drain span cleared within [%v, %v]",
				run, spans, before, after)
		}
	}
	assertSpans(t, "other cause", listRunHoldSpans(t, s, "run-c"), []domain.RunHoldSpan{
		holdSpan("run-c", nil, domain.HoldTrustBlocked, admissionEpoch, admissionEpoch, nil),
	})
}
