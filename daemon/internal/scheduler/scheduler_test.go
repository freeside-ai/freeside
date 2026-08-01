package scheduler_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func ptr[T any](v T) *T { return &v }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// clock is a settable test clock.
type clock struct{ now atomic.Value }

func newClock(start time.Time) *clock {
	c := &clock{}
	c.now.Store(start.UTC())
	return c
}
func (c *clock) Now() time.Time    { return c.now.Load().(time.Time) }
func (c *clock) Set(now time.Time) { c.now.Store(now.UTC()) }

var epoch = time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)

func openItem(t *testing.T, st *store.Store, id domain.ItemID, project domain.ProjectID) domain.AttentionItem {
	t.Helper()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: project,
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            "scheduler test subject",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(context.Background(), item)
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func deadlineSchedule(item domain.AttentionItem, fireAt time.Time) domain.Schedule {
	s, err := domain.NewSchedule(domain.ScheduleInput{
		ID: domain.ScheduleID("schedule-pr_checks_deadline-" + string(item.ID)), ProjectID: item.ProjectID,
		Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: ptr(item.ID), ItemVersion: ptr(item.ItemVersion),
		},
		CreatedAt: epoch, FireAt: ptr(fireAt),
	})
	if err != nil {
		panic(err)
	}
	return s
}

func janitorSchedule(intervalSeconds int64) domain.Schedule {
	s, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-janitor", ProjectID: "project-system",
		Kind:      domain.ScheduleJanitor,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: epoch, IntervalSeconds: ptr(intervalSeconds),
	})
	if err != nil {
		panic(err)
	}
	return s
}

func newScheduler(
	t *testing.T, st *store.Store, c *clock, mode domain.OperatingMode,
	kinds map[domain.ScheduleKind]scheduler.Registration,
) *scheduler.Scheduler {
	t.Helper()
	s, err := scheduler.New(st, mode, c.Now, kinds)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readSchedule(t *testing.T, st *store.Store, id domain.ScheduleID) domain.Schedule {
	t.Helper()
	var got domain.Schedule
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetSchedule(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func readOccurrence(
	t *testing.T, st *store.Store, id domain.ScheduleID, generation int64, nominal time.Time,
) domain.ScheduleOccurrence {
	t.Helper()
	var got domain.ScheduleOccurrence
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetScheduleOccurrence(context.Background(), id, generation, nominal)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestOneShotFiresAndTerminates is the §5.16 one-shot bullet under both
// operating modes: the deadline fires once its instant passes, the handler
// receives a trusted event carrying the expected generation and subject
// version, and the consumption terminates the schedule fired-and-handled.
func TestOneShotFiresAndTerminates(t *testing.T) {
	for _, mode := range domain.AllOperatingModes {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			c := newClock(epoch)
			item := openItem(t, st, "item-1", "project-1")
			sched := deadlineSchedule(item, epoch.Add(30*time.Minute))

			var events []domain.ScheduleEvent
			s := newScheduler(t, st, c, mode, map[domain.ScheduleKind]scheduler.Registration{
				domain.SchedulePRChecksDeadline: {Handle: func(
					_ context.Context, ev domain.ScheduleEvent, sc domain.Schedule,
				) (scheduler.Consumption, error) {
					events = append(events, ev)
					fired, err := sc.Concluded(domain.ScheduleFired, domain.ResolutionDeadlineElapsed, ev.FiredAt)
					if err != nil {
						return scheduler.Consumption{}, err
					}
					return scheduler.Consumption{Outcome: domain.OutcomeHandled, Schedule: &fired}, nil
				}},
			})
			if err := s.Arm(ctx, sched, *sched.FireAt); err != nil {
				t.Fatal(err)
			}

			// Not due yet: nothing fires.
			if err := s.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Fatalf("fired early: %+v", events)
			}

			c.Set(epoch.Add(31 * time.Minute))
			if err := s.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 {
				t.Fatalf("events = %+v", events)
			}
			ev := events[0]
			if ev.Generation != 1 || *ev.Subject.ItemVersion != item.ItemVersion ||
				!ev.NominalFireAt.Equal(*sched.FireAt) {
				t.Fatalf("event = %+v", ev)
			}
			got := readSchedule(t, st, sched.ID)
			if got.Status != domain.ScheduleFired || got.Resolution == nil ||
				got.Resolution.Reason != domain.ResolutionDeadlineElapsed {
				t.Fatalf("schedule = %+v", got)
			}
			occ := readOccurrence(t, st, sched.ID, 1, *sched.FireAt)
			if occ.Status != domain.OccurrenceConsumed || *occ.Outcome != domain.OutcomeHandled {
				t.Fatalf("occurrence = %+v", occ)
			}

			// Terminal schedules never fire again.
			c.Set(epoch.Add(2 * time.Hour))
			if err := s.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 {
				t.Fatalf("terminal refire: %+v", events)
			}
		})
	}
}

// TestOneShotConsumptionMustTerminate enforces the §5.16 termination bullet
// mechanically: a handler that leaves a one-shot armed in place is a
// correctness error, not a silent leak.
func TestOneShotConsumptionMustTerminate(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch.Add(time.Hour))
	item := openItem(t, st, "item-1", "project-1")
	sched := deadlineSchedule(item, epoch.Add(30*time.Minute))
	s := newScheduler(t, st, c, domain.ModeUnattended, map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline: {Handle: func(
			context.Context, domain.ScheduleEvent, domain.Schedule,
		) (scheduler.Consumption, error) {
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
	})
	if err := s.Arm(ctx, sched, *sched.FireAt); err != nil {
		t.Fatal(err)
	}
	err := s.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "must conclude or re-arm") {
		t.Fatalf("RunOnce = %v", err)
	}
	// The refused consumption left the occurrence pending: durably
	// redeliverable, never silently dropped.
	occ := readOccurrence(t, st, sched.ID, 1, *sched.FireAt)
	if occ.Status != domain.OccurrencePending {
		t.Fatalf("occurrence = %+v", occ)
	}
}

// TestRecurringCoalescesMissedFires is the §5.16 coalescing bullet: a
// recurring schedule that missed fires delivers one occurrence at the
// latest nominal instant with the gap recorded, and the clock advances past
// it.
func TestRecurringCoalescesMissedFires(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch)
	sched := janitorSchedule(30)

	var events []domain.ScheduleEvent
	s := newScheduler(t, st, c, domain.ModeAttendedDev, map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleJanitor: {Handle: func(
			_ context.Context, ev domain.ScheduleEvent, _ domain.Schedule,
		) (scheduler.Consumption, error) {
			events = append(events, ev)
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
	})
	if err := s.Arm(ctx, sched, epoch.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Ten intervals elapse unobserved (a stopped daemon): one fire, at the
	// latest nominal instant, carrying the nine missed occurrences.
	c.Set(epoch.Add(330 * time.Second))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	ev := events[0]
	wantNominal := epoch.Add(330 * time.Second)
	if !ev.NominalFireAt.Equal(wantNominal) {
		t.Fatalf("nominal = %s, want %s", ev.NominalFireAt, wantNominal)
	}
	if ev.Gap == nil || ev.Gap.MissedOccurrences != 10 ||
		!ev.Gap.EarliestMissedAt.Equal(epoch.Add(30*time.Second)) {
		t.Fatalf("gap = %+v", ev.Gap)
	}

	// The next interval fires cleanly with no gap.
	c.Set(wantNominal.Add(30 * time.Second))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Gap != nil ||
		!events[1].NominalFireAt.Equal(wantNominal.Add(30*time.Second)) {
		t.Fatalf("second fire = %+v", events[len(events)-1])
	}
}

// TestPendingOccurrenceRedelivers is the §5.16 transactional bullet from
// the scheduler's side: an occurrence left pending (a crash after the fire
// committed, before consumption) is redelivered on the next pass.
func TestPendingOccurrenceRedelivers(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch.Add(time.Hour))
	item := openItem(t, st, "item-1", "project-1")
	sched := deadlineSchedule(item, epoch.Add(30*time.Minute))

	handlerErr := errors.New("boom")
	fail := true
	var delivered []domain.ScheduleEvent
	handle := func(_ context.Context, ev domain.ScheduleEvent, sc domain.Schedule) (scheduler.Consumption, error) {
		if fail {
			return scheduler.Consumption{}, handlerErr
		}
		delivered = append(delivered, ev)
		fired, err := sc.Concluded(domain.ScheduleFired, domain.ResolutionDeadlineElapsed, ev.FiredAt)
		if err != nil {
			return scheduler.Consumption{}, err
		}
		return scheduler.Consumption{Outcome: domain.OutcomeHandled, Schedule: &fired}, nil
	}
	s := newScheduler(t, st, c, domain.ModeUnattended, map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline: {Handle: handle},
	})
	if err := s.Arm(ctx, sched, *sched.FireAt); err != nil {
		t.Fatal(err)
	}

	// First pass: the fire commits its pending occurrence, then the handler
	// fails; the scheduler surfaces the correctness error and the
	// occurrence stays pending.
	if err := s.RunOnce(ctx); !errors.Is(err, handlerErr) {
		t.Fatalf("RunOnce = %v", err)
	}
	occ := readOccurrence(t, st, sched.ID, 1, *sched.FireAt)
	if occ.Status != domain.OccurrencePending {
		t.Fatalf("occurrence after failure = %+v", occ)
	}

	// Second pass (a restarted scheduler): the same identity redelivers.
	fail = false
	s2 := newScheduler(t, st, c, domain.ModeUnattended, map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline: {Handle: handle},
	})
	if err := s2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || !delivered[0].NominalFireAt.Equal(*sched.FireAt) {
		t.Fatalf("delivered = %+v", delivered)
	}
	if got := readSchedule(t, st, sched.ID); got.Status != domain.ScheduleFired {
		t.Fatalf("schedule = %+v", got)
	}
}

// TestStaleEventReArms is the §5.16 stale-event fixture: a handler that
// finds its expectation stale re-arms under a new generation with the
// corrected binding, the stale pending occurrence is settled as
// stale_generation, and the next fire carries the new expectation.
func TestStaleEventReArms(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch.Add(31 * time.Minute))
	item := openItem(t, st, "item-1", "project-1")
	sched := deadlineSchedule(item, epoch.Add(30*time.Minute))

	// The subject moved after arming: bump the item's version so the
	// event's expectation (version 1) is stale at consumption.
	moved := item
	moved.ItemVersion = 2
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, moved)
	}); err != nil {
		t.Fatal(err)
	}

	var generations []int64
	s := newScheduler(t, st, c, domain.ModeUnattended, map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline: {Handle: func(
			_ context.Context, ev domain.ScheduleEvent, sc domain.Schedule,
		) (scheduler.Consumption, error) {
			generations = append(generations, ev.Generation)
			var current domain.AttentionItem
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				current, err = tx.GetAttentionItem(ctx, *ev.Subject.ItemID)
				return err
			}); err != nil {
				return scheduler.Consumption{}, err
			}
			if current.ItemVersion != *ev.Subject.ItemVersion {
				// Stale expectation: recompute and re-arm with the corrected
				// binding and a fresh deadline.
				subject := sc.Subject
				subject.ItemVersion = ptr(current.ItemVersion)
				reArmed, err := sc.ReArmed(subject, ptr(ev.FiredAt.Add(30*time.Minute)), ev.FiredAt)
				if err != nil {
					return scheduler.Consumption{}, err
				}
				return scheduler.Consumption{Outcome: domain.OutcomeReArmed, Schedule: &reArmed}, nil
			}
			fired, err := sc.Concluded(domain.ScheduleFired, domain.ResolutionDeadlineElapsed, ev.FiredAt)
			if err != nil {
				return scheduler.Consumption{}, err
			}
			return scheduler.Consumption{Outcome: domain.OutcomeHandled, Schedule: &fired}, nil
		}},
	})
	if err := s.Arm(ctx, sched, *sched.FireAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	occ := readOccurrence(t, st, sched.ID, 1, *sched.FireAt)
	if occ.Status != domain.OccurrenceConsumed || *occ.Outcome != domain.OutcomeReArmed {
		t.Fatalf("stale occurrence = %+v", occ)
	}
	got := readSchedule(t, st, sched.ID)
	if got.Generation != 2 || got.Status != domain.ScheduleArmed || *got.Subject.ItemVersion != 2 {
		t.Fatalf("re-armed = %+v", got)
	}

	// The re-armed deadline fires under the new generation and terminates.
	c.Set(got.FireAt.Add(time.Minute))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(generations) != 2 || generations[1] != 2 {
		t.Fatalf("generations = %v", generations)
	}
	if got := readSchedule(t, st, sched.ID); got.Status != domain.ScheduleFired {
		t.Fatalf("final = %+v", got)
	}
}

// TestExpiryTerminatesBeforeEventConstruction is the fire-time validation
// bullet's expiry leg: an expired schedule never constructs an event; it
// terminates durably with the recorded reason and its clock removed.
func TestExpiryTerminatesBeforeEventConstruction(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch)
	poll, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-installation_poll-4385298", ProjectID: "project-system",
		Kind: domain.ScheduleInstallationPoll,
		Subject: domain.ScheduleSubject{
			Type:           domain.ScheduleSubjectInstallationIntent,
			RegistrationID: ptr(int64(4385298)), ActiveEpoch: ptr(int64(1)),
			DurableIntentRevision: ptr(int64(2)),
		},
		CreatedAt: epoch, IntervalSeconds: ptr(int64(2)),
		ExpiresAt: ptr(epoch.Add(10 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	s := newScheduler(t, st, c, domain.ModeAttendedDev, map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleInstallationPoll: {
			Handle: func(context.Context, domain.ScheduleEvent, domain.Schedule) (scheduler.Consumption, error) {
				fired++
				return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
			},
			SubjectLive: func(context.Context, domain.Schedule) (bool, error) { return true, nil },
		},
	})
	if err := s.Arm(ctx, poll, epoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	c.Set(epoch.Add(11 * time.Minute))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("expired schedule fired %d times", fired)
	}
	got := readSchedule(t, st, poll.ID)
	if got.Status != domain.ScheduleExpired || got.Resolution.Reason != domain.ResolutionIntentExpired {
		t.Fatalf("schedule = %+v", got)
	}
}

// TestSubjectConclusionRecordsProof is the fire-time subject-existence leg:
// a watch over a concluded item resolves with recorded proof instead of
// firing or silently disappearing.
func TestSubjectConclusionRecordsProof(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch.Add(time.Hour))
	item := openItem(t, st, "item-1", "project-1")
	sched := deadlineSchedule(item, epoch.Add(30*time.Minute))
	fired := 0
	s := newScheduler(t, st, c, domain.ModeUnattended, map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline: {Handle: func(
			context.Context, domain.ScheduleEvent, domain.Schedule,
		) (scheduler.Consumption, error) {
			fired++
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
	})
	if err := s.Arm(ctx, sched, *sched.FireAt); err != nil {
		t.Fatal(err)
	}

	resolved := item
	resolved.ItemVersion = 2
	resolved.Status = domain.StatusResolved
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, resolved)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("concluded subject fired %d times", fired)
	}
	got := readSchedule(t, st, sched.ID)
	if got.Status != domain.ScheduleResolved || got.Resolution.Reason != domain.ResolutionSubjectConcluded {
		t.Fatalf("schedule = %+v", got)
	}
}

// TestObserveFailedKeepsRecurringAlive: a transient observation failure is
// an outcome, not an error — the occurrence settles, the schedule stays
// armed, and the next nominal fire retries.
func TestObserveFailedKeepsRecurringAlive(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch.Add(30 * time.Second))
	sched := janitorSchedule(30)
	outcomes := []domain.ScheduleOccurrenceOutcome{domain.OutcomeObserveFailed, domain.OutcomeHandled}
	fired := 0
	s := newScheduler(t, st, c, domain.ModeAttendedDev, map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleJanitor: {Handle: func(
			context.Context, domain.ScheduleEvent, domain.Schedule,
		) (scheduler.Consumption, error) {
			out := outcomes[fired]
			fired++
			return scheduler.Consumption{Outcome: out}, nil
		}},
	})
	if err := s.Arm(ctx, sched, epoch.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	occ := readOccurrence(t, st, sched.ID, 1, epoch.Add(30*time.Second))
	if *occ.Outcome != domain.OutcomeObserveFailed {
		t.Fatalf("occurrence = %+v", occ)
	}
	c.Set(epoch.Add(60 * time.Second))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fired != 2 {
		t.Fatalf("fired = %d", fired)
	}
	if got := readSchedule(t, st, sched.ID); got.Status != domain.ScheduleArmed {
		t.Fatalf("schedule = %+v", got)
	}
}

// TestStaleScanConcludesNothing: a concurrent re-arm between the due scan
// and the conclusion (an onboarding CLI replacing an expired installation
// intent while the daemon scheduler runs) must settle as a stale fire, not
// as an attempted transition from the superseded snapshot — which would be
// rejected as stale and stop the scheduler. The interleave is driven
// through the SubjectLive hook, which runs exactly in that window.
func TestStaleScanConcludesNothing(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch)
	poll, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-installation_poll-4385298", ProjectID: "project-system",
		Kind: domain.ScheduleInstallationPoll,
		Subject: domain.ScheduleSubject{
			Type:           domain.ScheduleSubjectInstallationIntent,
			RegistrationID: ptr(int64(4385298)), ActiveEpoch: ptr(int64(1)),
			DurableIntentRevision: ptr(int64(2)),
		},
		CreatedAt: epoch, IntervalSeconds: ptr(int64(2)),
		ExpiresAt: ptr(epoch.Add(10 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	var s *scheduler.Scheduler
	reArmDuringValidation := func(context.Context, domain.Schedule) (bool, error) {
		// The concurrent process replaces the intent: a fresh envelope under
		// the next revision, re-armed as the next generation.
		replacement := poll
		replacement.Subject.DurableIntentRevision = ptr(int64(3))
		replacement.ExpiresAt = ptr(c.Now().Add(10 * time.Minute))
		replacement.CreatedAt = c.Now()
		if err := s.Arm(ctx, replacement, c.Now().Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		// Report the scanned subject dead so the fire path attempts its
		// conclusion against the superseded snapshot.
		return false, nil
	}
	s = newScheduler(t, st, c, domain.ModeAttendedDev, map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleInstallationPoll: {
			Handle: func(context.Context, domain.ScheduleEvent, domain.Schedule) (scheduler.Consumption, error) {
				fired++
				return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
			},
			SubjectLive: reArmDuringValidation,
		},
	})
	if err := s.Arm(ctx, poll, epoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	c.Set(epoch.Add(3 * time.Second))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("stale scan fired %d times", fired)
	}
	got := readSchedule(t, st, poll.ID)
	if got.Generation != 2 || got.Status != domain.ScheduleArmed ||
		*got.Subject.DurableIntentRevision != 3 {
		t.Fatalf("current generation was disturbed: %+v", got)
	}
}

// TestAdvancedClockIsNotFiredEarly: when another scheduler process advances
// the shared clock between this process's due scan and its fire, the moved
// timer must read as not-due — firing from it would deliver the next
// nominal occurrence early and batch the cadence. The interleave is driven
// through the SubjectLive hook, which runs between the scan and the clock
// read.
func TestAdvancedClockIsNotFiredEarly(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch)
	poll, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-installation_poll-4385298", ProjectID: "project-system",
		Kind: domain.ScheduleInstallationPoll,
		Subject: domain.ScheduleSubject{
			Type:           domain.ScheduleSubjectInstallationIntent,
			RegistrationID: ptr(int64(4385298)), ActiveEpoch: ptr(int64(1)),
			DurableIntentRevision: ptr(int64(2)),
		},
		CreatedAt: epoch, IntervalSeconds: ptr(int64(2)),
		ExpiresAt: ptr(epoch.Add(10 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	fired := 0
	advanced := epoch.Add(5 * time.Second)
	advanceDuringValidation := func(context.Context, domain.Schedule) (bool, error) {
		// The other process consumed this nominal fire and advanced the
		// shared clock past now.
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.SetScheduleTimer(ctx, poll.ID, poll.Generation, advanced)
		}); err != nil {
			t.Fatal(err)
		}
		return true, nil
	}
	s := newScheduler(t, st, c, domain.ModeAttendedDev, map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleInstallationPoll: {
			Handle: func(context.Context, domain.ScheduleEvent, domain.Schedule) (scheduler.Consumption, error) {
				fired++
				return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
			},
			SubjectLive: advanceDuringValidation,
		},
	})
	if err := s.Arm(ctx, poll, epoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	c.Set(epoch.Add(3 * time.Second))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("advanced clock fired %d times", fired)
	}
	// The other process's clock is untouched.
	var next time.Time
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		_, next, _, err = tx.GetScheduleTimer(ctx, poll.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !next.Equal(advanced) {
		t.Fatalf("clock moved to %s, want %s", next, advanced)
	}
}

// TestStaleConsumptionRedelivers: a Commit that returns
// ErrStaleConsumption abandons the whole consumption — the transaction
// rolls back, the occurrence stays durably pending, the schedule is
// untouched, and no error surfaces — so the next pass revalidates the fire
// against the state that won the serialization.
func TestStaleConsumptionRedelivers(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch.Add(time.Hour))
	item := openItem(t, st, "item-1", "project-1")
	sched := deadlineSchedule(item, epoch.Add(30*time.Minute))
	stale := true
	handled := 0
	s := newScheduler(t, st, c, domain.ModeUnattended, map[domain.ScheduleKind]scheduler.Registration{
		domain.SchedulePRChecksDeadline: {Handle: func(
			_ context.Context, ev domain.ScheduleEvent, sc domain.Schedule,
		) (scheduler.Consumption, error) {
			fired, err := sc.Concluded(domain.ScheduleFired, domain.ResolutionDeadlineElapsed, ev.FiredAt)
			if err != nil {
				return scheduler.Consumption{}, err
			}
			return scheduler.Consumption{
				Outcome: domain.OutcomeHandled, Schedule: &fired,
				Commit: func(context.Context, *store.WriteTx) error {
					if stale {
						return scheduler.ErrStaleConsumption
					}
					handled++
					return nil
				},
			}, nil
		}},
	})
	if err := s.Arm(ctx, sched, *sched.FireAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	occ := readOccurrence(t, st, sched.ID, 1, *sched.FireAt)
	if occ.Status != domain.OccurrencePending {
		t.Fatalf("abandoned occurrence = %+v", occ)
	}
	if got := readSchedule(t, st, sched.ID); got.Status != domain.ScheduleArmed {
		t.Fatalf("abandoned schedule = %+v", got)
	}
	// The redelivered fire consumes once the state agrees.
	stale = false
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if handled != 1 {
		t.Fatalf("handled = %d", handled)
	}
	if got := readSchedule(t, st, sched.ID); got.Status != domain.ScheduleFired {
		t.Fatalf("final schedule = %+v", got)
	}
}

// TestConcurrentReArmMakesConsumptionStale: a re-arm that serializes
// between the handler's return and its consuming transaction (onboarding
// replacing an installation intent while the fire's handler runs) must
// settle the fire as stale — never write the handler's old-generation
// snapshot, whose rejected transition would stop the scheduler.
func TestConcurrentReArmMakesConsumptionStale(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch)
	poll, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-installation_poll-4385298", ProjectID: "project-system",
		Kind: domain.ScheduleInstallationPoll,
		Subject: domain.ScheduleSubject{
			Type:           domain.ScheduleSubjectInstallationIntent,
			RegistrationID: ptr(int64(4385298)), ActiveEpoch: ptr(int64(1)),
			DurableIntentRevision: ptr(int64(2)),
		},
		CreatedAt: epoch, IntervalSeconds: ptr(int64(2)),
		ExpiresAt: ptr(epoch.Add(10 * time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var s *scheduler.Scheduler
	s = newScheduler(t, st, c, domain.ModeAttendedDev, map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleInstallationPoll: {
			Handle: func(_ context.Context, ev domain.ScheduleEvent, sc domain.Schedule) (scheduler.Consumption, error) {
				// The concurrent process replaces the intent while this
				// handler holds the old snapshot, then the handler concludes
				// that old snapshot.
				replacement := poll
				replacement.Subject.DurableIntentRevision = ptr(int64(3))
				replacement.ExpiresAt = ptr(c.Now().Add(10 * time.Minute))
				replacement.CreatedAt = c.Now()
				if err := s.Arm(ctx, replacement, c.Now().Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				resolved, err := sc.Concluded(
					domain.ScheduleResolved, domain.ResolutionConditionSatisfied, ev.FiredAt)
				if err != nil {
					return scheduler.Consumption{}, err
				}
				return scheduler.Consumption{Outcome: domain.OutcomeHandled, Schedule: &resolved}, nil
			},
			SubjectLive: func(context.Context, domain.Schedule) (bool, error) { return true, nil },
		},
	})
	if err := s.Arm(ctx, poll, epoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	c.Set(epoch.Add(3 * time.Second))
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := readSchedule(t, st, poll.ID)
	if got.Generation != 2 || got.Status != domain.ScheduleArmed ||
		*got.Subject.DurableIntentRevision != 3 {
		t.Fatalf("current generation was disturbed: %+v", got)
	}
	occ := readOccurrence(t, st, poll.ID, 1, epoch.Add(2*time.Second))
	if occ.Status != domain.OccurrenceConsumed || *occ.Outcome != domain.OutcomeStaleGeneration {
		t.Fatalf("stale fire = %+v", occ)
	}
}

// TestArmConvergence: absent creates; an unchanged armed schedule keeps its
// generation and clock across restarts; a reconfigured shape re-arms under
// the next generation.
func TestArmConvergence(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := newClock(epoch)
	sched := janitorSchedule(30)
	s := newScheduler(t, st, c, domain.ModeAttendedDev, map[domain.ScheduleKind]scheduler.Registration{
		domain.ScheduleJanitor: {Handle: func(
			context.Context, domain.ScheduleEvent, domain.Schedule,
		) (scheduler.Consumption, error) {
			return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
		}},
	})
	if err := s.Arm(ctx, sched, epoch.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Restart with the same shape: generation 1 survives, and the clock is
	// not reset (the timer still aims at the original first fire).
	restarted := janitorSchedule(30)
	restarted.CreatedAt = epoch.Add(time.Hour)
	if err := s.Arm(ctx, restarted, epoch.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := readSchedule(t, st, sched.ID); got.Generation != 1 || !got.CreatedAt.Equal(epoch) {
		t.Fatalf("unchanged re-arm = %+v", got)
	}

	// A reconfigured cadence is a new generation.
	widened := janitorSchedule(60)
	widened.CreatedAt = epoch.Add(2 * time.Hour)
	if err := s.Arm(ctx, widened, epoch.Add(2*time.Hour+time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := readSchedule(t, st, sched.ID)
	if got.Generation != 2 || *got.IntervalSeconds != 60 {
		t.Fatalf("reconfigured = %+v", got)
	}
}
