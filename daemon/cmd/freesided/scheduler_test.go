package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func schedTestStore(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func schedTestResolvedPolicy(runID domain.RunID) domain.ResolvedPolicy {
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		},
	}})
	if err != nil {
		panic(err)
	}
	return policy
}

func schedTestItem(t *testing.T, st *store.Store) domain.AttentionItem {
	t.Helper()
	runID := domain.RunID("run-ready-1")
	policy := schedTestResolvedPolicy(runID)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-1", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(context.Background(), func(tx *store.WriteTx) error {
		ctx := context.Background()
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: item.ProjectID,
			SpecDigest: "sha256:scheduler-test-spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
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

// TestClaudeSchedulerRegistersEveryKind: the union is closed, and the
// production composition must own a handler for every member, so a kind
// added without a consumer registration fails here instead of sitting
// silently undriven.
func TestClaudeSchedulerRegistersEveryKind(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	wiring := &claudeComposition{
		janitor: &janitorSession{janitor: &stubJanitorRunner{}},
	}
	cfg := config{Claude: &claudeDriverConfig{OperatingMode: domain.ModeAttendedDev}}
	sched, err := newClaudeScheduler(st, cfg, wiring, func(context.Context) error { return nil }, func(context.Context) error { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[domain.ScheduleKind]bool{}
	for _, kind := range sched.RegisteredKinds() {
		registered[kind] = true
	}
	for _, kind := range domain.AllScheduleKinds {
		if !registered[kind] {
			t.Errorf("kind %s has no registration in the production composition", kind)
		}
	}
	if len(registered) != len(domain.AllScheduleKinds) {
		t.Errorf("registered %d kinds, union has %d", len(registered), len(domain.AllScheduleKinds))
	}
}

// Image upkeep runs as a trusted-config schedule at the doctor's cadence. A
// restart arms it again without resetting its clock.
func TestArmTrustedConfigJobsArmsImageUpkeepAndKeepsItsClock(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	cfg := config{
		Claude:         &claudeDriverConfig{OperatingMode: domain.ModeAttendedDev},
		DoctorInterval: 15 * time.Minute,
	}
	arm := func() domain.Schedule {
		t.Helper()
		wiring := &claudeComposition{
			janitor: &janitorSession{janitor: &stubJanitorRunner{}},
		}
		sched, err := newClaudeScheduler(st, cfg, wiring, func(context.Context) error { return nil }, func(context.Context) error { return nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := armTrustedConfigJobs(context.Background(), sched, cfg); err != nil {
			t.Fatal(err)
		}
		return readScheduleRow(t, st, imageUpkeepScheduleID)
	}
	first := arm()
	if first.Kind != domain.ScheduleImageUpkeep || first.IntervalSeconds == nil ||
		*first.IntervalSeconds != int64(cfg.DoctorInterval/time.Second) ||
		first.Subject.Type != domain.ScheduleSubjectTrustedConfig {
		t.Fatalf("image upkeep schedule = %+v, want a trusted-config job at the doctor's interval", first)
	}
	if again := arm(); !reflect.DeepEqual(again, first) {
		t.Fatalf("re-armed schedule = %+v, want the first one kept: %+v", again, first)
	}
}

// TestScheduledDoctorPassStartsUsageCollection: each scheduled doctor pass
// starts usage collection (plan §10), and a collection that cannot start
// fails nothing, because a failed doctor consumption stops the daemon.
func TestScheduledDoctorPassStartsUsageCollection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := schedTestStore(t)
	wiring := &claudeComposition{
		janitor:        &janitorSession{janitor: &stubJanitorRunner{}},
		runConformance: func(context.Context) error { return nil },
	}
	collections := 0
	sched, err := newClaudeScheduler(st, config{Claude: &claudeDriverConfig{OperatingMode: domain.ModeAttendedDev}},
		wiring, func(context.Context) error { return nil },
		func(context.Context) error {
			collections++
			return errors.New("usage pools unreadable")
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seconds := int64(3600)
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: doctorScheduleID, ProjectID: "project-system", Kind: domain.ScheduleDoctor,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: now.Add(-time.Hour), IntervalSeconds: &seconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sched.Arm(ctx, schedule, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sched.RunOnce(ctx); err != nil {
		t.Fatalf("a collection that could not start failed the doctor pass: %v", err)
	}
	if collections != 1 {
		t.Fatalf("scheduled doctor pass started %d collections, want 1", collections)
	}
}

// TestDeadlineReArmsOnStaleSubjectVersion is the §5.16 handler recheck for
// the deadline kinds: an event whose expected item version is stale (the
// base watch's fact write bumped it) re-arms under a new generation with
// the corrected binding and the same nominal deadline, so the elapsed wall
// time is never postponed, and the re-armed generation fires and terminates
// on the very next pass.
func TestDeadlineReArmsOnStaleSubjectVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := schedTestStore(t)
	item := schedTestItem(t, st)
	itemID := item.ID
	version := item.ItemVersion
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	fireAt := start.Add(30 * time.Minute)
	policy := schedTestResolvedPolicy(*item.Subject.RunID)
	sched, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        domain.ScheduleID("schedule-pr_checks_deadline-" + string(item.ID)),
		ProjectID: item.ProjectID, Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		RunID: item.Subject.RunID, PolicyDigest: &policy.Digest,
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
	t.Parallel()
	ctx := context.Background()
	st := schedTestStore(t)
	item := schedTestItem(t, st)
	itemID := item.ID
	version := item.ItemVersion
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	fireAt := start.Add(30 * time.Minute)
	policy := schedTestResolvedPolicy(*item.Subject.RunID)
	sched, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        domain.ScheduleID("schedule-review_wait_threshold-" + string(item.ID)),
		ProjectID: item.ProjectID, Kind: domain.ScheduleReviewWaitThreshold,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		RunID: item.Subject.RunID, PolicyDigest: &policy.Digest,
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

// deadlineFixture is a scheduler over both publication deadline kinds with
// an injected clock the test advances between passes.
type deadlineFixture struct {
	st   *store.Store
	item domain.AttentionItem
	s    *scheduler.Scheduler
	now  time.Time
}

func newDeadlineFixture(t *testing.T) *deadlineFixture {
	t.Helper()
	st := schedTestStore(t)
	f := &deadlineFixture{st: st, item: schedTestItem(t, st)}
	handler := deadlineRegistration(st)
	s, err := scheduler.New(st, domain.ModeUnattended,
		func() time.Time { return f.now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.SchedulePRChecksDeadline:    handler,
			domain.ScheduleReviewWaitThreshold: handler,
		})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	return f
}

// arm arms kind against the item's version 1, as armPublicationWatches does
// at start: the nominal deadline is start plus the kind's fixed duration.
func (f *deadlineFixture) arm(t *testing.T, kind domain.ScheduleKind, start time.Time) domain.ScheduleID {
	t.Helper()
	deadline, ok := engine.PublicationWatchDeadline(kind)
	if !ok {
		t.Fatalf("kind %s has no publication deadline", kind)
	}
	itemID, version := f.item.ID, f.item.ItemVersion
	fireAt := start.Add(deadline)
	policy := schedTestResolvedPolicy(*f.item.Subject.RunID)
	sched, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        engine.PublicationWatchScheduleID(kind, itemID),
		ProjectID: f.item.ProjectID, Kind: kind,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		RunID: f.item.Subject.RunID, PolicyDigest: &policy.Digest,
		CreatedAt: start, FireAt: &fireAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Arm(context.Background(), sched, fireAt); err != nil {
		t.Fatal(err)
	}
	return sched.ID
}

func (f *deadlineFixture) runAt(t *testing.T, now time.Time) {
	t.Helper()
	f.now = now
	if err := f.s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *deadlineFixture) readItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	var got domain.AttentionItem
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetAttentionItem(context.Background(), f.item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestDeadlineRecordsElapseOnOpenItem: a publication deadline firing on its
// still-open ready item appends the elapse to the item at its next version
// (#2752) and still terminates the schedule fired with deadline_elapsed.
func TestDeadlineRecordsElapseOnOpenItem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind    domain.ScheduleKind
		seconds int64
	}{
		{domain.SchedulePRChecksDeadline, 1800},
		{domain.ScheduleReviewWaitThreshold, 86400},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			f := newDeadlineFixture(t)
			start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
			id := f.arm(t, tc.kind, start)
			now := start.Add(time.Duration(tc.seconds)*time.Second + time.Second)
			f.runAt(t, now)

			got := f.readItem(t)
			want := []domain.ElapsedDeadline{{
				Kind: tc.kind, ArmedAt: start, DeadlineSeconds: tc.seconds, ElapsedAt: now,
			}}
			if !reflect.DeepEqual(got.ElapsedDeadlines, want) {
				t.Fatalf("elapsed deadlines = %+v, want %+v", got.ElapsedDeadlines, want)
			}
			if got.ItemVersion != f.item.ItemVersion+1 || got.Status != domain.StatusOpen {
				t.Fatalf("item version %d status %s, want %d open", got.ItemVersion, got.Status, f.item.ItemVersion+1)
			}
			sched := readScheduleRow(t, f.st, id)
			if sched.Status != domain.ScheduleFired ||
				sched.Resolution.Reason != domain.ResolutionDeadlineElapsed {
				t.Fatalf("deadline schedule = %+v", sched)
			}
		})
	}
}

// TestDeadlineRecordsBothElapsesInFiringOrder: the checks deadline fires
// first and bumps the item, so the review threshold's binding goes stale and
// re-arms under a new generation that keeps its nominal deadline. Its record
// therefore carries the same armed_at the first generation would have.
func TestDeadlineRecordsBothElapsesInFiringOrder(t *testing.T) {
	t.Parallel()
	f := newDeadlineFixture(t)
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	f.arm(t, domain.SchedulePRChecksDeadline, start)
	reviewID := f.arm(t, domain.ScheduleReviewWaitThreshold, start)

	checksAt := start.Add(31 * time.Minute)
	f.runAt(t, checksAt)
	reviewAt := start.Add(24*time.Hour + time.Minute)
	f.runAt(t, reviewAt) // the stale review binding re-arms
	if got := readScheduleRow(t, f.st, reviewID); got.Generation != 2 {
		t.Fatalf("review threshold generation = %d, want the re-armed 2", got.Generation)
	}
	f.runAt(t, reviewAt) // the re-armed generation fires

	got := f.readItem(t)
	want := []domain.ElapsedDeadline{
		{Kind: domain.SchedulePRChecksDeadline, ArmedAt: start, DeadlineSeconds: 1800, ElapsedAt: checksAt},
		{Kind: domain.ScheduleReviewWaitThreshold, ArmedAt: start, DeadlineSeconds: 86400, ElapsedAt: reviewAt},
	}
	if !reflect.DeepEqual(got.ElapsedDeadlines, want) {
		t.Fatalf("elapsed deadlines = %+v, want %+v", got.ElapsedDeadlines, want)
	}
	if got.ItemVersion != f.item.ItemVersion+2 {
		t.Fatalf("item version = %d, want %d", got.ItemVersion, f.item.ItemVersion+2)
	}
}

// TestDeadlineLeavesConcludedItemUnchanged: a deadline whose item concluded
// before it fired records the conclusion on the schedule and writes nothing
// to the item.
func TestDeadlineLeavesConcludedItemUnchanged(t *testing.T) {
	t.Parallel()
	f := newDeadlineFixture(t)
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	id := f.arm(t, domain.SchedulePRChecksDeadline, start)
	concluded := f.item
	concluded.ItemVersion++
	concluded.Status = domain.StatusDismissed
	concluded = domaintest.MustConclude(concluded)
	if err := f.st.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(context.Background(), concluded)
	}); err != nil {
		t.Fatal(err)
	}
	before := f.readItem(t)

	f.runAt(t, start.Add(31*time.Minute))
	if got := f.readItem(t); !reflect.DeepEqual(got, before) {
		t.Fatalf("concluded item changed:\n got %+v\nwant %+v", got, before)
	}
	sched := readScheduleRow(t, f.st, id)
	if sched.Status != domain.ScheduleResolved ||
		sched.Resolution.Reason != domain.ResolutionSubjectConcluded {
		t.Fatalf("deadline schedule = %+v", sched)
	}
}

// TestDeadlineOnClosedWorkflowConcludesWithoutTheItem: an abandoned task's
// closed episode refuses every item put, and its retirement can lag. A
// deadline firing on its still-open ready item records the elapse on the
// schedule alone instead of failing the handler, which would stop the
// scheduler.
func TestDeadlineOnClosedWorkflowConcludesWithoutTheItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDeadlineFixture(t)
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	id := f.arm(t, domain.SchedulePRChecksDeadline, start)
	var task domain.Task
	if err := f.st.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, *f.item.Subject.RunID)
		if err != nil {
			return err
		}
		if err := tx.RecordTaskStart(ctx, run.ID, start); err != nil {
			return err
		}
		if task, err = tx.GetTask(ctx, run.TaskID); err != nil {
			return err
		}
		_, err = tx.AbandonTask(ctx, task.ID, start.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.CloseTaskWorkflow(ctx, task.ID, task.CurrentStart().Ordinal, start.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := f.readItem(t)
	if before.Status != domain.StatusOpen {
		t.Fatalf("item status = %s, want open in the closed episode", before.Status)
	}

	f.runAt(t, start.Add(31*time.Minute))
	if got := f.readItem(t); !reflect.DeepEqual(got, before) {
		t.Fatalf("closed-episode item changed:\n got %+v\nwant %+v", got, before)
	}
	sched := readScheduleRow(t, f.st, id)
	if sched.Status != domain.ScheduleFired ||
		sched.Resolution.Reason != domain.ResolutionDeadlineElapsed {
		t.Fatalf("deadline schedule = %+v", sched)
	}
}

// TestDeadlineFiredBeforeItsDeadlineRedelivers: a redelivery after the wall
// clock stepped back can carry a fire instant before the nominal deadline.
// The consumption abandons itself to redeliver later rather than write a
// record the item would refuse. The scheduler never fires early on its own
// clock, so the handler is driven directly.
func TestDeadlineFiredBeforeItsDeadlineRedelivers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDeadlineFixture(t)
	start := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	id := f.arm(t, domain.SchedulePRChecksDeadline, start)
	sched := readScheduleRow(t, f.st, id)
	ev := domain.ScheduleEvent{
		ScheduleID: sched.ID, ProjectID: sched.ProjectID, Kind: sched.Kind,
		Generation: sched.Generation, Subject: sched.Subject,
		NominalFireAt: *sched.FireAt, FiredAt: sched.FireAt.Add(-time.Second),
	}
	consumption, err := deadlineRegistration(f.st).Handle(ctx, ev, sched)
	if err != nil {
		t.Fatal(err)
	}
	err = f.st.Write(ctx, func(tx *store.WriteTx) error { return consumption.Commit(ctx, tx) })
	if !errors.Is(err, scheduler.ErrStaleConsumption) {
		t.Fatalf("early fire commit = %v, want ErrStaleConsumption", err)
	}
	if got := f.readItem(t); got.ElapsedDeadlines != nil || got.ItemVersion != f.item.ItemVersion {
		t.Fatalf("early fire wrote the item: %+v", got)
	}
}

// seedCaptureRun persists the run and resolved policy the declaration
// re-gate re-derives from (an empty declared scope pairs with a policy
// declaring no paths key), so capture fixtures satisfy the reconstruction
// gates the production flow satisfies by construction.
func seedCaptureRun(t *testing.T, st *store.Store, runID domain.RunID) {
	t.Helper()
	ctx := context.Background()
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{Source: "override", Digest: domain.Digest("sha256:" + strings.Repeat("ab", 32))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1",
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy)
	}); err != nil {
		t.Fatal(err)
	}
}

type readyBindingAuthority struct {
	invocationID            domain.InvocationID
	publicationInvocationID domain.InvocationID
	identity                domain.Digest
}

func seedReadyBindingAuthority(
	t *testing.T, st *store.Store, runID domain.RunID, headSHA string,
) readyBindingAuthority {
	t.Helper()
	ctx := context.Background()
	var run domain.Run
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	invocationID := domain.InvocationID("inv-ready-" + string(runID))
	stageID := domain.StageID("stage-ready-" + string(runID))
	attemptID := domain.AttemptID("attempt-ready-" + string(runID))
	run.Stages = append(run.Stages, domain.Stage{
		ID: stageID, RunID: runID, Name: "ready binding authority",
		Attempts: []domain.Attempt{{
			ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID,
		}},
	})
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "ready-binding-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:ready-input",
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 424242, BaseRef: "main", BaseSHA: "deadbeef",
		},
		Workspace: "ready-binding-workspace", AdmittedAt: activeResourceTestTime.Add(-3 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: headSHA,
		ManifestDigest: "sha256:ready-manifest", RecordedAt: activeResourceTestTime.Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo: "owner/repo", BaseRef: "main", SourceHeadSHA: headSHA,
		ArtifactDigests: []domain.Digest{"sha256:ready-artifact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := publish.Outcome{
		Identity: identity.Digest(), Repo: "owner/repo", BaseRef: "main", HeadSHA: headSHA,
		Branch: identity.BranchName(), PRNumber: 450, EvidenceEligible: true,
	}
	payload, err := outcome.Encode()
	if err != nil {
		t.Fatal(err)
	}
	publicationInvocationID := domain.InvocationID("publish-production-" + string(runID))
	intentPayload, err := (publish.Intent{
		FormatVersion: publish.IntentFormatCurrent,
		Identity:      identity.Digest(), InvocationID: publicationInvocationID,
		Branch: identity.BranchName(),
		Repo:   "owner/repo", BaseRef: "main", SourceHeadSHA: headSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("cd", 32)),
		ProducingInvocationID: invocationID, ReservationRunID: runID,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	intentKey, err := publish.IntentKey(publicationInvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionExport(ctx, export); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, publish.IntentKindPublication, intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, publish.OutcomeKey(identity), publish.IntentKindOutcome, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return readyBindingAuthority{
		invocationID: invocationID, publicationInvocationID: publicationInvocationID,
		identity: identity.Digest(),
	}
}

// capturedRun seeds the store with a declared, PR-bound work unit and
// returns the ready item whose subject names its run, so the
// active-resource reconciler's §5.18 completion pass has a unit to settle.
func capturedRun(t *testing.T, st *store.Store) domain.AttentionItem {
	return capturedRunNamed(t, st, "run-cap", "item-ready-cap")
}

func capturedRunNamed(
	t *testing.T, st *store.Store, runID domain.RunID, itemID domain.ItemID,
) domain.AttentionItem {
	boundIssue := 443
	return capturedRunWithCriterion(
		t, st, runID, itemID, domain.CompletionBoundIssueClosedByMergedPR, &boundIssue,
	)
}

func capturedRunWithCriterion(
	t *testing.T, st *store.Store, runID domain.RunID, itemID domain.ItemID,
	criterion domain.CompletionCriterionKind, boundIssue *int,
) domain.AttentionItem {
	t.Helper()
	return capturedRunAtHead(t, st, runID, itemID, criterion, boundIssue, "cafed00d")
}

// capturedRunAtHead is capturedRunWithCriterion publishing head, for a test
// whose coordinates must validate as full SHAs.
func capturedRunAtHead(
	t *testing.T, st *store.Store, runID domain.RunID, itemID domain.ItemID,
	criterion domain.CompletionCriterionKind, boundIssue *int, head string,
) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	seedCaptureRun(t, st, runID)
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: criterion,
		BoundIssue:          boundIssue,
	}, runID, "project-1", time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.WorkUnitPRBinding{
		UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
		PRNumber: 450, BaseRef: "main", HeadSHA: head,
		RecordedAt: time.Date(2026, 2, 3, 3, 30, 0, 0, time.UTC),
	}
	authority := seedReadyBindingAuthority(t, st, runID, binding.HeadSHA)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		return tx.RecordWorkUnitPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		// PRHeadSHA is the head anchor the capture pass verifies the
		// binding against.
		PRHeadSHA:   head,
		PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, domain.ReadyItemPRBinding{
			ItemID: item.ID, RunID: runID,
			ProducingInvocationID: authority.invocationID, PublicationIdentity: authority.identity,
			PublicationInvocationID: authority.publicationInvocationID,
			Repo:                    binding.Repo,
			RepositoryID:            binding.RepositoryID, PRNumber: binding.PRNumber,
			BaseRef: binding.BaseRef, HeadSHA: binding.HeadSHA,
			RecordedAt: binding.RecordedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	// Publication convergence records the ready milestone alongside the ready
	// item; the completion mirror is written only over a standing ready.
	publicationInvocation := authority.publicationInvocationID
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestonePublicationReady,
			InvocationID: &publicationInvocation, RecordedAt: binding.RecordedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func readCaptureState(t *testing.T, st *store.Store) ([]domain.PullMergeFact, []domain.IssueStateFact, *domain.WorkUnitCompletion) {
	t.Helper()
	ctx := context.Background()
	var (
		pulls      []domain.PullMergeFact
		issues     []domain.IssueStateFact
		completion *domain.WorkUnitCompletion
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if pulls, err = tx.ListPullMergeFacts(ctx, 424242, 450); err != nil {
			return err
		}
		if issues, err = tx.ListIssueStateFacts(ctx, 424242, 443); err != nil {
			return err
		}
		c, err := tx.GetWorkUnitCompletion(ctx, domain.WorkUnitIDForRun("run-cap"))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		completion = &c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return pulls, issues, completion
}
