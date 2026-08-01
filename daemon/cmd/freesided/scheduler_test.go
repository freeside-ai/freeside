package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func schedTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func schedTestItem(t *testing.T, st *store.Store) domain.AttentionItem {
	t.Helper()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-1", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
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

func readScheduleRow(t *testing.T, st *store.Store, id domain.ScheduleID) domain.Schedule {
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

func watchSchedule(t *testing.T, item domain.AttentionItem) domain.Schedule {
	t.Helper()
	itemID := item.ID
	version := item.ItemVersion
	interval := int64(60)
	s, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        domain.ScheduleID("schedule-base_advance_watch-" + string(item.ID)),
		ProjectID: item.ProjectID, Kind: domain.ScheduleBaseAdvanceWatch,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		CreatedAt:       time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC),
		IntervalSeconds: &interval,
		BaseWatch: &domain.ScheduleBaseWatch{
			Repo: "owner/repo", BaseRef: "main", AdmittedBaseSHA: "cafebabe",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestBaseAdvanceWatchMaintainsFact is the issue-named consumer: the watch
// writes the item's base-freshness fact on material change only, so an
// unchanged observation leaves the item version alone while a base advance
// bumps it (invalidating commands prepared against the stale base claim).
func TestBaseAdvanceWatchMaintainsFact(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := schedTestItem(t, st)
	sched := watchSchedule(t, item)
	start := sched.CreatedAt

	observed := "cafebabe"
	var observeErr error
	now := start
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st,
				func(context.Context, domain.ScheduleBaseWatch) (string, error) {
					return observed, observeErr
				}),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	readItem := func() domain.AttentionItem {
		t.Helper()
		var got domain.AttentionItem
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, err = tx.GetAttentionItem(ctx, item.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// First fire: the fact appears (fresh base), and the version bumps once
	// for the fact's introduction.
	now = start.Add(61 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := readItem()
	if got.BaseFreshness == nil || got.BaseFreshness.Advanced ||
		got.BaseFreshness.ObservedBaseSHA != "cafebabe" || got.ItemVersion != 2 {
		t.Fatalf("first fact = %+v (version %d)", got.BaseFreshness, got.ItemVersion)
	}
	// The fact-writing consumption re-armed the watch with the binding it
	// created: the schedule expects the bumped item version (§5.16 recheck).
	if s1 := readScheduleRow(t, st, sched.ID); s1.Generation != 2 || *s1.Subject.ItemVersion != 2 {
		t.Fatalf("post-fact schedule = gen %d expecting v%v", s1.Generation, *s1.Subject.ItemVersion)
	}

	// Second fire, unchanged tip: no item churn.
	now = start.Add(121 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readItem(); got.ItemVersion != 2 {
		t.Fatalf("unchanged observation churned item to version %d", got.ItemVersion)
	}

	// Transient observation failure: an outcome, not an error; the schedule
	// stays armed.
	observeErr = errors.New("github unreachable")
	now = start.Add(181 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		// The first fact write re-armed the watch with its corrected binding
		// (generation 2, nominal fires at 61s+60s cadence).
		occ, err := tx.GetScheduleOccurrence(ctx, sched.ID, 2, start.Add(181*time.Second))
		if err != nil {
			return err
		}
		if *occ.Outcome != domain.OutcomeObserveFailed {
			t.Fatalf("failure occurrence = %+v", occ)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The base advances: the fact flips and the version bumps.
	observeErr = nil
	observed = "deadbeef"
	now = start.Add(241 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = readItem()
	if got.BaseFreshness == nil || !got.BaseFreshness.Advanced ||
		got.BaseFreshness.ObservedBaseSHA != "deadbeef" || got.ItemVersion != 3 {
		t.Fatalf("advanced fact = %+v (version %d)", got.BaseFreshness, got.ItemVersion)
	}

	// A concluded item resolves the watch with recorded proof at the next
	// fire instead of firing the handler.
	concluded := got
	concluded.ItemVersion++
	concluded.Status = domain.StatusResolved
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, concluded)
	}); err != nil {
		t.Fatal(err)
	}
	now = start.Add(301 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var final domain.Schedule
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		final, err = tx.GetSchedule(ctx, sched.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if final.Status != domain.ScheduleResolved ||
		final.Resolution.Reason != domain.ResolutionSubjectConcluded {
		t.Fatalf("final schedule = %+v", final)
	}
}

// TestDeadlineReArmsOnStaleSubjectVersion is the §5.16 handler recheck for
// the deadline kinds: an event whose expected item version is stale (the
// base watch's fact write bumped it) re-arms under a new generation with
// the corrected binding and the same nominal deadline, so the elapsed wall
// time is never postponed, and the re-armed generation fires and terminates
// on the very next pass.
func TestDeadlineReArmsOnStaleSubjectVersion(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := schedTestItem(t, st)
	itemID := item.ID
	version := item.ItemVersion
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	fireAt := start.Add(30 * time.Minute)
	sched, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        domain.ScheduleID("schedule-pr_checks_deadline-" + string(item.ID)),
		ProjectID: item.ProjectID, Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		CreatedAt: start, FireAt: &fireAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := fireAt.Add(time.Second)
	s, err := scheduler.New(st, domain.ModeUnattended,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.SchedulePRChecksDeadline: deadlineRegistration(st),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, fireAt); err != nil {
		t.Fatal(err)
	}

	// The subject moved after arming (still open, higher version).
	moved := item
	moved.ItemVersion = 3
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, moved)
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := readScheduleRow(t, st, sched.ID)
	if got.Generation != 2 || got.Status != domain.ScheduleArmed ||
		*got.Subject.ItemVersion != 3 || !got.FireAt.Equal(fireAt) {
		t.Fatalf("re-armed = %+v", got)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		occ, err := tx.GetScheduleOccurrence(ctx, sched.ID, 1, fireAt)
		if err != nil {
			return err
		}
		if *occ.Outcome != domain.OutcomeReArmed {
			t.Fatalf("stale occurrence = %+v", occ)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The corrected generation fires immediately and terminates.
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = readScheduleRow(t, st, sched.ID)
	if got.Status != domain.ScheduleFired ||
		got.Resolution.Reason != domain.ResolutionDeadlineElapsed {
		t.Fatalf("final = %+v", got)
	}
}

// TestDeadlineRegistrationTerminatesFired: a publication deadline that
// fires with its item still open terminates fired-and-handled with
// deadline_elapsed on the synced aggregate.
func TestDeadlineRegistrationTerminatesFired(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := schedTestItem(t, st)
	itemID := item.ID
	version := item.ItemVersion
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	fireAt := start.Add(30 * time.Minute)
	sched, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        domain.ScheduleID("schedule-review_wait_threshold-" + string(item.ID)),
		ProjectID: item.ProjectID, Kind: domain.ScheduleReviewWaitThreshold,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		CreatedAt: start, FireAt: &fireAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := fireAt.Add(time.Second)
	s, err := scheduler.New(st, domain.ModeUnattended,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleReviewWaitThreshold: deadlineRegistration(st),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, fireAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var got domain.Schedule
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetSchedule(ctx, sched.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ScheduleFired ||
		got.Resolution.Reason != domain.ResolutionDeadlineElapsed {
		t.Fatalf("deadline schedule = %+v", got)
	}
}

// TestBaseAdvanceWatchRechecksSubjectOnObserveFailure: the §5.16 subject
// recheck runs even when the GitHub observation fails — a concluded item
// resolves with recorded proof, and a stale expectation re-arms with the
// corrected binding, instead of consuming observe_failed against old state.
func TestBaseAdvanceWatchRechecksSubjectOnObserveFailure(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := schedTestItem(t, st)
	sched := watchSchedule(t, item)
	failing := func(context.Context, domain.ScheduleBaseWatch) (string, error) {
		return "", errors.New("github unreachable")
	}

	// A moved (still open) item under a failing observer re-arms with the
	// corrected binding.
	moved := item
	moved.ItemVersion = 3
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, moved)
	}); err != nil {
		t.Fatal(err)
	}
	now := sched.CreatedAt.Add(2 * time.Minute)
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st, failing),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, sched.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := readScheduleRow(t, st, sched.ID)
	if got.Generation != 2 || got.Status != domain.ScheduleArmed || *got.Subject.ItemVersion != 3 {
		t.Fatalf("re-armed = %+v", got)
	}

	// A concluded item under a failing observer resolves with proof.
	concluded := moved
	concluded.ItemVersion++
	concluded.Status = domain.StatusResolved
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, concluded)
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got = readScheduleRow(t, st, sched.ID)
	if got.Status != domain.ScheduleResolved ||
		got.Resolution.Reason != domain.ResolutionSubjectConcluded {
		t.Fatalf("final = %+v", got)
	}
}

// TestBaseAdvanceWatchResolvesConcludedItemImmediately: an item that
// concluded between fire-time validation and the handler's read resolves
// the watch with recorded proof on this fire, not a cadence later. The
// race window is driven through a SubjectLive override that reports the
// subject live while the stored item is already terminal.
func TestBaseAdvanceWatchResolvesConcludedItemImmediately(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := schedTestItem(t, st)
	concluded := item
	concluded.ItemVersion++
	concluded.Status = domain.StatusResolved
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, concluded)
	}); err != nil {
		t.Fatal(err)
	}
	sched := watchSchedule(t, item)
	registration := baseAdvanceRegistration(st,
		func(context.Context, domain.ScheduleBaseWatch) (string, error) {
			return "cafebabe", nil
		})
	registration.SubjectLive = func(context.Context, domain.Schedule) (bool, error) {
		return true, nil
	}
	now := sched.CreatedAt.Add(2 * time.Minute)
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: registration,
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, sched.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := readScheduleRow(t, st, sched.ID)
	if got.Status != domain.ScheduleResolved ||
		got.Resolution.Reason != domain.ResolutionSubjectConcluded {
		t.Fatalf("schedule = %+v", got)
	}
}
