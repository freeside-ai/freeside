package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var activeResourceTestTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

func activeReadyItem(t *testing.T, st *store.Store) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	runID := domain.RunID("run-active")
	seedCaptureRun(t, st, runID)
	authority := seedReadyBindingAuthority(t, st, runID, "cafed00d")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-active", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "published and verified", RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss,
		},
		PRHeadSHA: "cafed00d", ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
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
			Repo:                    "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: item.PRHeadSHA,
			RecordedAt: activeResourceTestTime.Add(-time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func armActiveTestSchedules(t *testing.T, st *store.Store, item domain.AttentionItem) {
	t.Helper()
	ctx := context.Background()
	itemID, version := item.ID, item.ItemVersion
	runID := *item.Subject.RunID
	policy := schedTestResolvedPolicy(runID)
	deadline := activeResourceTestTime.Add(time.Hour)
	interval := int64(60)
	inputs := []domain.ScheduleInput{
		{
			ID:        engineScheduleID(domain.SchedulePRChecksDeadline, item.ID),
			ProjectID: item.ProjectID, Kind: domain.SchedulePRChecksDeadline,
			Subject: domain.ScheduleSubject{Type: domain.ScheduleSubjectAttentionItem, ItemID: &itemID, ItemVersion: &version},
			RunID:   &runID, PolicyDigest: &policy.Digest, CreatedAt: activeResourceTestTime, FireAt: &deadline,
		},
		{
			ID:        engineScheduleID(domain.ScheduleReviewWaitThreshold, item.ID),
			ProjectID: item.ProjectID, Kind: domain.ScheduleReviewWaitThreshold,
			Subject: domain.ScheduleSubject{Type: domain.ScheduleSubjectAttentionItem, ItemID: &itemID, ItemVersion: &version},
			RunID:   &runID, PolicyDigest: &policy.Digest, CreatedAt: activeResourceTestTime, FireAt: &deadline,
		},
		{
			ID:        engineScheduleID(domain.ScheduleBaseAdvanceWatch, item.ID),
			ProjectID: item.ProjectID, Kind: domain.ScheduleBaseAdvanceWatch,
			Subject: domain.ScheduleSubject{Type: domain.ScheduleSubjectAttentionItem, ItemID: &itemID, ItemVersion: &version},
			RunID:   &runID, PolicyDigest: &policy.Digest, CreatedAt: activeResourceTestTime,
			IntervalSeconds: &interval,
			BaseWatch:       &domain.ScheduleBaseWatch{Repo: "owner/repo", BaseRef: "main", AdmittedBaseSHA: "cafebabe"},
		},
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, input := range inputs {
			schedule, err := domain.NewSchedule(input)
			if err != nil {
				return err
			}
			if err := tx.PutSchedule(ctx, schedule); err != nil {
				return err
			}
			var next time.Time
			if schedule.FireAt != nil {
				next = *schedule.FireAt
			} else {
				next = schedule.CreatedAt.Add(time.Duration(*schedule.IntervalSeconds) * time.Second)
			}
			if err := tx.SetScheduleTimer(ctx, schedule.ID, schedule.Generation, next); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func engineScheduleID(kind domain.ScheduleKind, itemID domain.ItemID) domain.ScheduleID {
	return domain.ScheduleID("schedule-" + string(kind) + "-" + string(itemID))
}

func readActiveItem(t *testing.T, st *store.Store, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func exactPull(state string, merged bool) publish.PullObservation {
	observation := publish.PullObservation{
		Number: 450, State: state, BaseRepoID: 424242,
		BaseRef: "main", HeadSHA: "cafed00d",
	}
	if merged {
		observation.Merged = true
		observation.MergeCommitSHA = "deadbeef"
	}
	return observation
}

func TestActiveResourceReconcileWaitsForBoundIssueThenConcludes(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	armActiveTestSchedules(t, st, item)
	pullCalls := 0
	issueClosed := false
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			return exactPull("closed", true), nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			if !issueClosed {
				return publish.IssueObservation{Number: 443, State: "open"}, nil
			}
			return publish.IssueObservation{
				Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef",
			}, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	failures, err := reconciler.Reconcile(ctx)
	if err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion {
		t.Fatalf("item concluded before issue closure = %+v", got)
	}
	for _, id := range publicationScheduleIDs(item.ID) {
		if schedule := readScheduleRow(t, st, id); schedule.Status != domain.ScheduleArmed {
			t.Fatalf("schedule %s settled before issue closure = %+v", id, schedule)
		}
	}
	pulls, issues, completion := readCaptureState(t, st)
	if len(pulls) != 1 || !pulls[0].Merged || len(issues) != 1 ||
		issues[0].State != domain.IssueOpen || completion != nil {
		t.Fatalf("pending capture = pulls %v issues %v completion %v", pulls, issues, completion)
	}

	issueClosed = true
	if failures, err = reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("issue-closure Reconcile = %v, %v", failures, err)
	}
	got = readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusResolved || got.ItemVersion != item.ItemVersion+1 {
		t.Fatalf("item = %+v", got)
	}
	for _, id := range publicationScheduleIDs(item.ID) {
		schedule := readScheduleRow(t, st, id)
		if schedule.Status != domain.ScheduleResolved || schedule.Resolution == nil ||
			schedule.Resolution.Reason != domain.ResolutionSubjectConcluded {
			t.Fatalf("schedule %s = %+v", id, schedule)
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			_, _, ok, err := tx.GetScheduleTimer(ctx, id)
			if err != nil {
				return err
			}
			if ok {
				t.Fatalf("schedule %s retained its timer", id)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	pulls, issues, completion = readCaptureState(t, st)
	if len(pulls) != 1 || !pulls[0].Merged || len(issues) != 2 || completion == nil {
		t.Fatalf("capture = pulls %v issues %v completion %v", pulls, issues, completion)
	}
	// A fresh reconciler after restart sees the terminal item, settles no-op
	// schedules, and never polls the concluded resource again.
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("restart Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 4 { // two active passes, each with a repository-identity recheck
		t.Fatalf("pull calls after terminal replay = %d, want 4", pullCalls)
	}
}

func TestActiveResourceCompletionUsesPersistedSharedPRFacts(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	first := capturedRunNamed(t, st, "run-shared-first", "item-shared-first")
	now := activeResourceTestTime
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			return exactPull("closed", true), nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			return publish.IssueObservation{
				Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef",
			}, nil
		},
		now: func() time.Time { return now },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("first Reconcile = %v, %v", failures, err)
	}
	if got := readActiveItem(t, st, first.ID); got.Status != domain.StatusResolved {
		t.Fatalf("first item = %+v", got)
	}

	second := capturedRunNamed(t, st, "run-shared-second", "item-shared-second")
	now = now.Add(time.Hour)
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("shared-pr Reconcile = %v, %v", failures, err)
	}
	if got := readActiveItem(t, st, second.ID); got.Status != domain.StatusResolved {
		t.Fatalf("second item = %+v", got)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		completion, err := tx.GetWorkUnitCompletion(ctx, domain.WorkUnitIDForRun("run-shared-second"))
		if err != nil {
			return err
		}
		if !completion.RecordedAt.Equal(activeResourceTestTime) {
			t.Fatalf("completion recorded_at = %s, want persisted fact time %s",
				completion.RecordedAt, activeResourceTestTime)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestActiveResourcePRCompletionIgnoresOptionalBoundIssue(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	boundIssue := 443
	item := capturedRunWithCriterion(
		t, st, "run-pr-only", "item-pr-only",
		domain.CompletionBoundPRMerged, &boundIssue,
	)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			return exactPull("closed", true), nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusResolved {
		t.Fatalf("item = %+v", got)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		completion, err := tx.GetWorkUnitCompletion(ctx, domain.WorkUnitIDForRun("run-pr-only"))
		if err != nil {
			return err
		}
		if completion.Criterion != domain.CompletionBoundPRMerged || completion.BoundIssue != nil {
			t.Fatalf("completion = %+v", completion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestActiveResourceHonorsHistoricalCompletionAfterIssueReopens(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	pull := domain.PullMergeFact{
		Repo: "owner/repo", RepositoryID: 424242, PRNumber: 450,
		State: domain.PullRequestClosed, Merged: true, MergeCommitSHA: "deadbeef",
		BaseRef: "main", HeadSHA: "cafed00d", ObservedAt: activeResourceTestTime.Add(-time.Hour),
	}
	issue := domain.IssueStateFact{
		Repo: "owner/repo", RepositoryID: 424242, IssueNumber: 443,
		State: domain.IssueClosed, ClosedByCommitSHA: "deadbeef",
		ObservedAt: activeResourceTestTime.Add(-time.Hour),
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, *item.Subject.RunID)
		if err != nil {
			return err
		}
		binding, err := tx.GetWorkUnitPRBinding(ctx, declaration.ID)
		if err != nil {
			return err
		}
		if _, err := tx.AppendPullMergeFact(ctx, pull); err != nil {
			return err
		}
		if _, err := tx.AppendIssueStateFact(ctx, issue); err != nil {
			return err
		}
		completion, ok := domain.EvaluateWorkUnitCompletion(declaration, binding, pull, &issue)
		if !ok {
			return errors.New("fixture did not derive completion")
		}
		if err := tx.RecordWorkUnitCompletion(ctx, completion); err != nil {
			return err
		}
		reopened := issue
		reopened.State = domain.IssueOpen
		reopened.ClosedByCommitSHA = ""
		reopened.ObservedAt = issue.ObservedAt.Add(30 * time.Minute)
		_, err = tx.AppendIssueStateFact(ctx, reopened)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			return exactPull("closed", true), nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusResolved {
		t.Fatalf("item = %+v", got)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetWorkUnitCompletion(ctx, domain.WorkUnitIDForRun(*item.Subject.RunID))
		return err
	}); err != nil {
		t.Fatalf("historical completion became unreadable: %v", err)
	}
}

func TestActiveResourceReconcileUnmergedCloseConcludesUndeclaredItem(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			return exactPull("closed", false), nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	failures, err := reconciler.Reconcile(ctx)
	if err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusResolved {
		t.Fatalf("item status = %s", got.Status)
	}
	var facts []domain.PullMergeFact
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		facts, err = tx.ListPullMergeFacts(ctx, 424242, 450)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].Merged || facts[0].State != domain.PullRequestClosed {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestActiveResourceReconcileRequiresExactReturnedResource(t *testing.T) {
	t.Run("response number", func(t *testing.T) {
		ctx := context.Background()
		st := schedTestStore(t)
		item := activeReadyItem(t, st)
		reconciler := activeResourceReconciler{
			store: st,
			pull: func(context.Context, string, int) (publish.PullObservation, error) {
				observed := exactPull("closed", true)
				observed.Number++
				return observed, nil
			},
			now: func() time.Time { return activeResourceTestTime },
		}
		failures, err := reconciler.Reconcile(ctx)
		if err != nil || len(failures) != 1 {
			t.Fatalf("Reconcile = %v, %v", failures, err)
		}
		if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
			t.Fatalf("mismatched response concluded item = %+v", got)
		}
	})

	t.Run("repository identity", func(t *testing.T) {
		ctx := context.Background()
		st := schedTestStore(t)
		item := activeReadyItem(t, st)
		reconciler := activeResourceReconciler{
			store: st,
			pull: func(context.Context, string, int) (publish.PullObservation, error) {
				observed := exactPull("closed", true)
				observed.BaseRepoID++
				return observed, nil
			},
			now: func() time.Time { return activeResourceTestTime },
		}
		failures, err := reconciler.Reconcile(ctx)
		if err != nil || len(failures) != 0 {
			t.Fatalf("Reconcile = %v, %v", failures, err)
		}
		if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
			t.Fatalf("foreign repository concluded item = %+v", got)
		}
	})
}

func TestActiveResourceReconcileRetriesTransientFailureWithoutChurn(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	transient := errors.New("temporary forge failure")
	fail := true
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			if fail {
				return publish.PullObservation{}, transient
			}
			return exactPull("open", false), nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	failures, err := reconciler.Reconcile(ctx)
	if err != nil || len(failures) != 1 || !errors.Is(failures[0], transient) {
		t.Fatalf("failed Reconcile = %v, %v", failures, err)
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion {
		t.Fatalf("failed observation changed item = %+v", got)
	}
	fail = false
	if failures, err = reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("retry Reconcile = %v, %v", failures, err)
	}
	var before store.ServerState
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		before, err = tx.ServerState(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if failures, err = reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("unchanged Reconcile = %v, %v", failures, err)
	}
	var after store.ServerState
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		after, err = tx.ServerState(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("unchanged observation advanced revision %d -> %d", before.Revision, after.Revision)
	}
}

func TestActiveResourceReconcileSettlesReturnedAndDismissedItemsWithoutPolling(t *testing.T) {
	for _, status := range []domain.ItemStatus{domain.StatusResolved, domain.StatusDismissed} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := activeReadyItem(t, st)
			armActiveTestSchedules(t, st, item)
			item.Status = status
			item.ItemVersion++
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, item)
			}); err != nil {
				t.Fatal(err)
			}
			pullCalls := 0
			reconciler := activeResourceReconciler{
				store: st,
				pull: func(context.Context, string, int) (publish.PullObservation, error) {
					pullCalls++
					return exactPull("open", false), nil
				},
				now: func() time.Time { return activeResourceTestTime },
			}
			if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
				t.Fatalf("Reconcile = %v, %v", failures, err)
			}
			if pullCalls != 0 {
				t.Fatalf("terminal item polled %d times", pullCalls)
			}
			for _, id := range publicationScheduleIDs(item.ID) {
				if schedule := readScheduleRow(t, st, id); schedule.Status != domain.ScheduleResolved {
					t.Fatalf("schedule %s = %+v", id, schedule)
				}
			}
		})
	}
}
