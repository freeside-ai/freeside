package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func schedTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
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

func watchSchedule(t *testing.T, item domain.AttentionItem) domain.Schedule {
	t.Helper()
	itemID := item.ID
	version := item.ItemVersion
	interval := int64(60)
	policy := schedTestResolvedPolicy(*item.Subject.RunID)
	s, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        domain.ScheduleID("schedule-base_advance_watch-" + string(item.ID)),
		ProjectID: item.ProjectID, Kind: domain.ScheduleBaseAdvanceWatch,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		RunID: item.Subject.RunID, PolicyDigest: &policy.Digest,
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
				}, mergeCapture{}),
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
	// fire; the handler's concluded path (which also hosts the final
	// capture pass) records the same subject_concluded proof the built-in
	// check used to.
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

// TestClaudeSchedulerRegistersEveryKind: the union is closed, and the
// production composition must own a handler for every member, so a kind
// added without a consumer registration fails here instead of sitting
// silently undriven.
func TestClaudeSchedulerRegistersEveryKind(t *testing.T) {
	st := schedTestStore(t)
	wiring := &claudeComposition{
		janitor:        &janitorSession{janitor: &stubJanitorRunner{}},
		observeBaseTip: staticBaseObserver,
	}
	cfg := config{Claude: &claudeDriverConfig{OperatingMode: domain.ModeAttendedDev}}
	sched, err := newClaudeScheduler(st, cfg, wiring, func(context.Context) error { return nil })
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
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st, failing, mergeCapture{}),
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
		}, mergeCapture{})
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

// capturedRun seeds the store with a declared, PR-bound work unit and
// returns the ready item whose subject names its run, so the base-advance
// watch's §5.18 capture pass has a unit to settle.
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
		Identity: identity.Digest(), InvocationID: publicationInvocationID,
		Repo: "owner/repo", BaseRef: "main", SourceHeadSHA: headSHA,
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
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
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
		PRNumber: 450, BaseRef: "main", HeadSHA: "cafed00d",
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
		PRHeadSHA:   "cafed00d",
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

// TestBaseAdvanceWatchCapturesMergeCompletion (§5.18, issue #443): the
// watch's capture pass appends the bound PR's state on material change,
// observes the bound issue once the PR merges, and records the write-once
// completion exactly when the declared criterion is satisfied; a settled
// unit stops observing.
func TestBaseAdvanceWatchCapturesMergeCompletion(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	sched := watchSchedule(t, item)
	start := sched.CreatedAt

	pull := publish.PullObservation{Number: 450, State: "open", BaseRef: "main", BaseRepoID: 424242, HeadSHA: "cafed00d"}
	issue := publish.IssueObservation{Number: 443, State: "open"}
	pullCalls, issueCalls := 0, 0
	capture := mergeCapture{
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			return pull, nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			issueCalls++
			return issue, nil
		},
	}
	now := start
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st,
				func(context.Context, domain.ScheduleBaseWatch) (string, error) {
					return "cafebabe", nil
				}, capture),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// First fire: the open PR's state is recorded; the issue is not yet
	// observed (the criterion evaluates it only against a merge).
	now = start.Add(61 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pulls, issues, completion := readCaptureState(t, st)
	if len(pulls) != 1 || pulls[0].Merged || pulls[0].State != domain.PullRequestOpen {
		t.Fatalf("first capture pulls = %+v", pulls)
	}
	if len(issues) != 0 || completion != nil || issueCalls != 0 {
		t.Fatalf("premature issue/completion capture: %v %v %d", issues, completion, issueCalls)
	}

	// Second fire, unchanged: no new fact rows.
	now = start.Add(121 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if pulls, _, _ := readCaptureState(t, st); len(pulls) != 1 {
		t.Fatalf("unchanged observation appended: %+v", pulls)
	}

	// The PR merges and the issue closes by that merge commit: the facts
	// append and the completion records in the same consumption.
	pull = publish.PullObservation{
		Number: 450, State: "closed", BaseRef: "main", BaseRepoID: 424242,
		HeadSHA: "cafed00d", Merged: true, MergeCommitSHA: "deadbeef",
	}
	issue = publish.IssueObservation{Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef"}
	now = start.Add(181 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pulls, issues, completion = readCaptureState(t, st)
	if len(pulls) != 2 || !pulls[1].Merged || pulls[1].MergeCommitSHA != "deadbeef" {
		t.Fatalf("merged capture pulls = %+v", pulls)
	}
	if len(issues) != 1 || issues[0].State != domain.IssueClosed || issues[0].ClosedByCommitSHA != "deadbeef" {
		t.Fatalf("issue capture = %+v", issues)
	}
	if completion == nil || completion.Criterion != domain.CompletionBoundIssueClosedByMergedPR ||
		completion.MergeCommitSHA != "deadbeef" || completion.BoundIssue == nil || *completion.BoundIssue != 443 {
		t.Fatalf("completion = %+v", completion)
	}

	// A settled unit stops observing: the next fire runs no capture reads
	// and appends nothing.
	before := pullCalls
	now = start.Add(241 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if pullCalls != before {
		t.Fatalf("settled unit was re-observed (%d -> %d)", before, pullCalls)
	}
	if pulls, issues, _ := readCaptureState(t, st); len(pulls) != 2 || len(issues) != 1 {
		t.Fatalf("settled unit appended: %v %v", pulls, issues)
	}
}

// TestBaseAdvanceWatchFinalCaptureOnConcludedItem: the operator merges and
// immediately concludes the item; the watch's next fire is the final
// capture pass. A failed observation leaves the schedule armed (the capture
// is retried, not lost); the successful pass records the completion and
// resolves the schedule in one consumption.
func TestBaseAdvanceWatchFinalCaptureOnConcludedItem(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	concluded := item
	concluded.ItemVersion++
	concluded.Status = domain.StatusResolved
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, concluded)
	}); err != nil {
		t.Fatal(err)
	}
	sched := watchSchedule(t, item)
	start := sched.CreatedAt

	pullErr := errors.New("github unreachable")
	capture := mergeCapture{
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			if pullErr != nil {
				return publish.PullObservation{}, pullErr
			}
			return publish.PullObservation{
				Number: 450, State: "closed", BaseRef: "main", BaseRepoID: 424242,
				HeadSHA: "cafed00d", Merged: true, MergeCommitSHA: "deadbeef",
			}, nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			return publish.IssueObservation{Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef"}, nil
		},
	}
	now := start
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st,
				func(context.Context, domain.ScheduleBaseWatch) (string, error) {
					return "cafebabe", nil
				}, capture),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Failed final observation: the schedule stays armed and retries
	// rather than resolving uncaptured.
	now = start.Add(61 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readScheduleRow(t, st, sched.ID); got.Status != domain.ScheduleArmed {
		t.Fatalf("schedule resolved uncaptured: %+v", got)
	}

	// Successful final observation: capture and resolution commit together.
	pullErr = nil
	now = start.Add(121 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readScheduleRow(t, st, sched.ID); got.Status != domain.ScheduleResolved ||
		got.Resolution.Reason != domain.ResolutionSubjectConcluded {
		t.Fatalf("schedule = %+v", got)
	}
	pulls, issues, completion := readCaptureState(t, st)
	if len(pulls) != 1 || len(issues) != 1 || completion == nil {
		t.Fatalf("final capture state = %v %v %v", pulls, issues, completion)
	}
}

// TestBaseAdvanceWatchCaptureRecordsForeignIdentityWithoutCompleting: a
// repository name re-bound to a different repository (rename plus
// name reuse) records the observed repository's facts under the OBSERVED
// numeric identity, never completes the unit on them, and never reads the
// bound issue through the unverified name.
func TestBaseAdvanceWatchCaptureRecordsForeignIdentityWithoutCompleting(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	sched := watchSchedule(t, item)
	start := sched.CreatedAt

	issueCalls := 0
	capture := mergeCapture{
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			// A merged PR with the right number and base ref, in a
			// repository that is not the bound one.
			return publish.PullObservation{
				Number: 450, State: "closed", BaseRef: "main", BaseRepoID: 999,
				HeadSHA: "cafed00d", Merged: true, MergeCommitSHA: "deadbeef",
			}, nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			issueCalls++
			return publish.IssueObservation{Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef"}, nil
		},
	}
	now := start
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st,
				func(context.Context, domain.ScheduleBaseWatch) (string, error) {
					return "cafebabe", nil
				}, capture),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = start.Add(61 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if issueCalls != 0 {
		t.Fatalf("bound issue was read through an unverified repository name (%d calls)", issueCalls)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetWorkUnitCompletion(ctx, domain.WorkUnitIDForRun("run-cap")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("foreign merge completed the unit: %v", err)
		}
		boundFacts, err := tx.ListPullMergeFacts(ctx, 424242, 450)
		if err != nil {
			return err
		}
		if len(boundFacts) != 0 {
			t.Fatalf("foreign observation recorded under the bound identity: %+v", boundFacts)
		}
		observed, err := tx.ListPullMergeFacts(ctx, 999, 450)
		if err != nil {
			return err
		}
		if len(observed) != 1 || !observed[0].Merged {
			t.Fatalf("observed facts = %+v, want the foreign repository's merge recorded honestly", observed)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestBaseAdvanceWatchDeclaredUnboundRetriesInsteadOfResolving: a declared
// unit whose PR binding has not yet converged (the crash window between
// publication passes) is never "nothing to record": the concluded item's
// watch stays armed and retries until the binding lands, then captures and
// resolves.
func TestBaseAdvanceWatchDeclaredUnboundRetriesInsteadOfResolving(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	runID := domain.RunID("run-cap")
	seedCaptureRun(t, st, runID)
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
	}, runID, "project-1", time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	}); err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-cap", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		PRHeadSHA:         "cafed00d",
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	item.ItemVersion++
	item.Status = domain.StatusResolved
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	sched := watchSchedule(t, item)
	start := sched.CreatedAt

	capture := mergeCapture{
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			return publish.PullObservation{
				Number: 450, State: "closed", BaseRef: "main", BaseRepoID: 424242,
				HeadSHA: "cafed00d", Merged: true, MergeCommitSHA: "deadbeef",
			}, nil
		},
	}
	now := start
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st,
				func(context.Context, domain.ScheduleBaseWatch) (string, error) {
					return "cafebabe", nil
				}, capture),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Declared but unbound: the watch must not resolve.
	now = start.Add(61 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readScheduleRow(t, st, sched.ID); got.Status != domain.ScheduleArmed {
		t.Fatalf("schedule resolved while the unit was unbound: %+v", got)
	}

	// The binding converges (a later publication pass); the next fire
	// captures and resolves in one consumption.
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitPRBinding(ctx, domain.WorkUnitPRBinding{
			UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: "cafed00d",
			RecordedAt: time.Date(2026, 2, 3, 3, 30, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatal(err)
	}
	now = start.Add(121 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := readScheduleRow(t, st, sched.ID); got.Status != domain.ScheduleResolved {
		t.Fatalf("schedule = %+v, want resolved after the bound capture", got)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetWorkUnitCompletion(ctx, declaration.ID)
		return err
	}); err != nil {
		t.Fatalf("completion after bound capture: %v", err)
	}
}

// TestBaseAdvanceWatchCaptureRefusesBindingOffTheAnchors: a reconstructed
// binding that does not restate the pass's first-party anchors (here the
// ready item's published head) fails the observation, so the concluded
// item's watch retries rather than capturing through coordinates the
// engine never published.
func TestBaseAdvanceWatchCaptureRefusesBindingOffTheAnchors(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	// The item's published head moves off the binding's (a corrupt or
	// foreign binding row would present the same disagreement).
	mismatched := item
	mismatched.ItemVersion++
	mismatched.Status = domain.StatusResolved
	mismatched.PRHeadSHA = "0ther5ha"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, mismatched)
	}); err != nil {
		t.Fatal(err)
	}
	sched := watchSchedule(t, item)
	start := sched.CreatedAt
	pullCalls := 0
	capture := mergeCapture{
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			return publish.PullObservation{
				Number: 450, State: "closed", BaseRef: "main", BaseRepoID: 424242,
				HeadSHA: "cafed00d", Merged: true, MergeCommitSHA: "deadbeef",
			}, nil
		},
	}
	now := start
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st,
				func(context.Context, domain.ScheduleBaseWatch) (string, error) {
					return "cafebabe", nil
				}, capture),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = start.Add(61 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if pullCalls != 0 {
		t.Fatalf("the pass observed through an unanchored binding (%d pulls)", pullCalls)
	}
	if got := readScheduleRow(t, st, sched.ID); got.Status != domain.ScheduleArmed {
		t.Fatalf("schedule resolved through an unanchored binding: %+v", got)
	}
	if pulls, _, completion := readCaptureState(t, st); len(pulls) != 0 || completion != nil {
		t.Fatalf("unanchored binding recorded: %v %v", pulls, completion)
	}
}
