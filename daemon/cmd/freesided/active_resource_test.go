package main

import (
	"context"
	"errors"
	"path/filepath"
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

func assertIdentityInvalidated(
	t *testing.T, st *store.Store, original domain.AttentionItem,
	bound, observed string,
) {
	t.Helper()
	got := readActiveItem(t, st, original.ID)
	if got.Status != domain.StatusSuperseded || got.ItemVersion != original.ItemVersion+1 {
		t.Fatalf("identity-invalidated item = status %s version %d", got.Status, got.ItemVersion)
	}
	inv := got.ReadinessInvalidation
	if inv == nil || inv.Reason != domain.ReadinessInvalidationIdentityChanged ||
		inv.Bound != bound || inv.Observed != observed ||
		!inv.ObservedAt.Equal(activeResourceTestTime) {
		t.Fatalf("identity invalidation = %+v", inv)
	}
	if len(got.EvidenceSnapshot) != 0 {
		t.Fatalf("identity invalidation created completion evidence: %+v", got.EvidenceSnapshot)
	}
	for _, id := range publicationScheduleIDs(original.ID) {
		if schedule := readScheduleRow(t, st, id); !schedule.Status.Terminal() {
			t.Fatalf("schedule %s not concluded: %s", id, schedule.Status)
		}
	}
}

func activePullFacts(
	t *testing.T, st *store.Store, repositoryID int64, prNumber int,
) []domain.PullMergeFact {
	t.Helper()
	var facts []domain.PullMergeFact
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		facts, err = tx.ListPullMergeFacts(context.Background(), repositoryID, prNumber)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return facts
}

func assertNoActiveCompletion(t *testing.T, st *store.Store, item domain.AttentionItem) {
	t.Helper()
	err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		_, err := tx.GetWorkUnitCompletion(
			context.Background(), domain.WorkUnitIDForRun(*item.Subject.RunID),
		)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("completion lookup = %v, want ErrNotFound", err)
	}
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
	cases := []struct {
		name            string
		mutate          func(*publish.PullObservation)
		observed        string
		foreignRepoID   int64
		foreignPRNumber int
	}{
		{
			name: "response number",
			mutate: func(observed *publish.PullObservation) {
				observed.Number = 451
			},
			observed: "424242#451", foreignRepoID: 424242, foreignPRNumber: 451,
		},
		{
			name: "repository identity before fact validation",
			mutate: func(observed *publish.PullObservation) {
				observed.BaseRepoID = 434343
				observed.State = "not-a-pull-state"
				observed.BaseRef = ""
				observed.HeadSHA = ""
			},
			observed: "434343#450", foreignRepoID: 434343, foreignPRNumber: 450,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := activeReadyItem(t, st)
			armActiveTestSchedules(t, st, item)
			reconciler := activeResourceReconciler{
				store: st,
				pull: func(context.Context, string, int) (publish.PullObservation, error) {
					observed := exactPull("closed", true)
					tc.mutate(&observed)
					return observed, nil
				},
				now: func() time.Time { return activeResourceTestTime },
			}
			failures, err := reconciler.Reconcile(ctx)
			if err != nil || len(failures) != 0 {
				t.Fatalf("Reconcile = %v, %v", failures, err)
			}
			assertIdentityInvalidated(t, st, item, "424242#450", tc.observed)
			assertNoActiveCompletion(t, st, item)
			if facts := activePullFacts(t, st, 424242, 450); len(facts) != 0 {
				t.Fatalf("bound pull facts = %+v, want none", facts)
			}
			if facts := activePullFacts(t, st, tc.foreignRepoID, tc.foreignPRNumber); len(facts) != 0 {
				t.Fatalf("foreign pull facts = %+v, want none", facts)
			}
		})
	}
}

func TestActiveResourceReconcileRetriesMalformedReturnedIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*publish.PullObservation)
	}{
		{name: "missing repository ID", mutate: func(p *publish.PullObservation) { p.BaseRepoID = 0 }},
		{name: "negative repository ID", mutate: func(p *publish.PullObservation) { p.BaseRepoID = -1 }},
		{name: "missing pull number", mutate: func(p *publish.PullObservation) { p.Number = 0 }},
		{name: "negative pull number", mutate: func(p *publish.PullObservation) { p.Number = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := activeReadyItem(t, st)
			armActiveTestSchedules(t, st, item)
			malformed := true
			reconciler := activeResourceReconciler{
				store: st,
				pull: func(context.Context, string, int) (publish.PullObservation, error) {
					observed := exactPull("open", false)
					if malformed {
						tc.mutate(&observed)
					}
					return observed, nil
				},
				now: func() time.Time { return activeResourceTestTime },
			}
			failures, err := reconciler.Reconcile(ctx)
			if err != nil || len(failures) != 1 || !errors.Is(failures[0], domain.ErrNonPositive) {
				t.Fatalf("malformed Reconcile = %v, %v", failures, err)
			}
			got := readActiveItem(t, st, item.ID)
			if got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion ||
				got.ReadinessInvalidation != nil {
				t.Fatalf("malformed identity changed item = %+v", got)
			}
			for _, id := range publicationScheduleIDs(item.ID) {
				if schedule := readScheduleRow(t, st, id); schedule.Status != domain.ScheduleArmed {
					t.Fatalf("malformed identity concluded schedule %s: %s", id, schedule.Status)
				}
			}
			if facts := activePullFacts(t, st, 424242, 450); len(facts) != 0 {
				t.Fatalf("malformed identity persisted pull facts: %+v", facts)
			}

			malformed = false
			if failures, err = reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
				t.Fatalf("retry Reconcile = %v, %v", failures, err)
			}
			if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen ||
				got.ItemVersion != item.ItemVersion || got.ReadinessInvalidation != nil {
				t.Fatalf("valid retry changed ready item = %+v", got)
			}
		})
	}
}

func TestActiveResourceReconcileSameIdentityRenameStaysReady(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			observed := exactPull("open", false)
			observed.BaseRepo = "renamed-owner/renamed-repo"
			return observed, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion ||
		got.ReadinessInvalidation != nil {
		t.Fatalf("same-ID rename invalidated ready item = %+v", got)
	}
}

// TestActiveResourceReconcileInvalidatesReadyOnBindingDivergence covers #496's
// same-PR candidate changes and #514's identity divergence. Either withdraws
// the ready claim, records the precise readiness-invalidation reason, and
// concludes the publication schedules so a stale pass cannot remain actionable.
func TestActiveResourceReconcileInvalidatesReadyOnBindingDivergence(t *testing.T) {
	cases := []struct {
		name            string
		mutate          func(*publish.PullObservation)
		reason          domain.ReadinessInvalidationReason
		bound, observed string
	}{
		{
			name:   "head advanced",
			mutate: func(p *publish.PullObservation) { p.HeadSHA = "f00dbabe" },
			reason: domain.ReadinessInvalidationHeadChanged,
			bound:  "cafed00d", observed: "f00dbabe",
		},
		{
			name:   "retargeted",
			mutate: func(p *publish.PullObservation) { p.BaseRef = "release" },
			reason: domain.ReadinessInvalidationRetargeted,
			bound:  "main", observed: "release",
		},
		{
			name:   "repository identity",
			mutate: func(p *publish.PullObservation) { p.BaseRepoID = 434343 },
			reason: domain.ReadinessInvalidationIdentityChanged,
			bound:  "424242#450", observed: "434343#450",
		},
		{
			name:   "pull number",
			mutate: func(p *publish.PullObservation) { p.Number = 451 },
			reason: domain.ReadinessInvalidationIdentityChanged,
			bound:  "424242#450", observed: "424242#451",
		},
		{
			name: "repository identity and pull number",
			mutate: func(p *publish.PullObservation) {
				p.BaseRepoID = 434343
				p.Number = 451
			},
			reason: domain.ReadinessInvalidationIdentityChanged,
			bound:  "424242#450", observed: "434343#451",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := activeReadyItem(t, st)
			armActiveTestSchedules(t, st, item)
			reconciler := activeResourceReconciler{
				store: st,
				pull: func(context.Context, string, int) (publish.PullObservation, error) {
					observed := exactPull("open", false)
					tc.mutate(&observed)
					return observed, nil
				},
				now: func() time.Time { return activeResourceTestTime },
			}
			failures, err := reconciler.Reconcile(ctx)
			if err != nil || len(failures) != 0 {
				t.Fatalf("Reconcile = %v, %v", failures, err)
			}
			got := readActiveItem(t, st, item.ID)
			if got.Status != domain.StatusSuperseded || got.ItemVersion != item.ItemVersion+1 {
				t.Fatalf("invalidated item = status %s version %d", got.Status, got.ItemVersion)
			}
			inv := got.ReadinessInvalidation
			if inv == nil || inv.Reason != tc.reason ||
				inv.Bound != tc.bound || inv.Observed != tc.observed ||
				!inv.ObservedAt.Equal(activeResourceTestTime) {
				t.Fatalf("invalidation = %+v", inv)
			}
			// The publication watches are concluded alongside the supersession,
			// so no later fire acts on the invalidated item.
			for _, id := range publicationScheduleIDs(item.ID) {
				if sc := readScheduleRow(t, st, id); !sc.Status.Terminal() {
					t.Fatalf("schedule %s not concluded: %s", id, sc.Status)
				}
			}
		})
	}
}

func TestActiveResourceIdentityInvalidationAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	options := store.Options{AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}}
	st, err := store.Open(ctx, dbPath, options)
	if err != nil {
		t.Fatal(err)
	}
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(ctx, dbPath, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reconciler := activeResourceReconciler{
		store: reopened,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			observed := exactPull("open", false)
			observed.BaseRepoID = 434343
			return observed, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("restart Reconcile = %v, %v", failures, err)
	}
	assertIdentityInvalidated(t, reopened, item, "424242#450", "434343#450")
	if facts := activePullFacts(t, reopened, 434343, 450); len(facts) != 0 {
		t.Fatalf("foreign pull facts after restart = %+v, want none", facts)
	}
}

func TestActiveResourceIdentityInvalidationRejectsChangedBindingAtCommit(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			observed := exactPull("open", false)
			observed.Number = 451
			return observed, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	observation, err := reconciler.observe(ctx, item, activeResourceTestTime)
	if err != nil {
		t.Fatal(err)
	}
	observation.binding.PRNumber++ // simulates a replacement between observe and commit
	if err := reconciler.commit(ctx, observation); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("commit = %v, want ErrImmutableConflict", err)
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion {
		t.Fatalf("binding race changed item = %+v", got)
	}
	for _, id := range publicationScheduleIDs(item.ID) {
		if schedule := readScheduleRow(t, st, id); schedule.Status != domain.ScheduleArmed {
			t.Fatalf("binding race concluded schedule %s: %s", id, schedule.Status)
		}
	}
}

func TestActiveResourceIdentityInvalidationYieldsToConcurrentSupersession(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			observed := exactPull("open", false)
			observed.BaseRepoID = 434343
			return observed, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	observation, err := reconciler.observe(ctx, item, activeResourceTestTime)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := readActiveItem(t, st, item.ID)
	concurrent.Status = domain.StatusSuperseded
	concurrent.ItemVersion++
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, concurrent)
	}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.commit(ctx, observation); err != nil {
		t.Fatal(err)
	}
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusSuperseded || got.ItemVersion != concurrent.ItemVersion ||
		got.ReadinessInvalidation != nil {
		t.Fatalf("concurrent supersession did not win = %+v", got)
	}
	for _, id := range publicationScheduleIDs(item.ID) {
		if schedule := readScheduleRow(t, st, id); !schedule.Status.Terminal() {
			t.Fatalf("schedule %s not concluded after concurrent supersession: %s", id, schedule.Status)
		}
	}
}

func TestActiveResourceIdentityDivergenceAtCompletionRecheck(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	armActiveTestSchedules(t, st, item)
	pullCalls := 0
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			observed := exactPull("closed", true)
			if pullCalls == 2 {
				observed.BaseRepoID = 434343
				observed.Number = 451
				observed.State = "not-a-pull-state"
				observed.BaseRef = "foreign-base"
				observed.HeadSHA = "foreign-head"
				observed.MergeCommitSHA = "foreign-merge"
			}
			return observed, nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			return publish.IssueObservation{
				// The identity recheck takes precedence over fact validation, so
				// this malformed foreign issue response cannot strand readiness.
				Number: 999, State: "not-an-issue-state",
			}, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	assertIdentityInvalidated(t, st, item, "424242#450", "434343#451")
	pulls, issues, completion := readCaptureState(t, st)
	if len(pulls) != 1 || pulls[0].RepositoryID != 424242 || pulls[0].PRNumber != 450 {
		t.Fatalf("bound pull facts = %+v, want the initial exact observation", pulls)
	}
	if len(issues) != 0 || completion != nil {
		t.Fatalf("recheck divergence persisted issue/completion: issues=%+v completion=%+v", issues, completion)
	}
	if facts := activePullFacts(t, st, 434343, 451); len(facts) != 0 {
		t.Fatalf("foreign recheck facts = %+v, want none", facts)
	}
}

func TestActiveResourceMalformedIdentityAtCompletionRecheckRetriesWithoutChurn(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*publish.PullObservation)
	}{
		{name: "missing repository ID", mutate: func(p *publish.PullObservation) { p.BaseRepoID = 0 }},
		{name: "missing pull number", mutate: func(p *publish.PullObservation) { p.Number = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := capturedRun(t, st)
			armActiveTestSchedules(t, st, item)
			pullCalls := 0
			malformed := true
			reconciler := activeResourceReconciler{
				store: st,
				pull: func(context.Context, string, int) (publish.PullObservation, error) {
					pullCalls++
					observed := exactPull("closed", true)
					if malformed && pullCalls == 2 {
						tc.mutate(&observed)
					}
					return observed, nil
				},
				issue: func(context.Context, string, int) (publish.IssueObservation, error) {
					return publish.IssueObservation{
						Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef",
					}, nil
				},
				now: func() time.Time { return activeResourceTestTime },
			}
			failures, err := reconciler.Reconcile(ctx)
			if err != nil || len(failures) != 1 || !errors.Is(failures[0], domain.ErrNonPositive) {
				t.Fatalf("malformed recheck Reconcile = %v, %v", failures, err)
			}
			got := readActiveItem(t, st, item.ID)
			if got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion ||
				got.ReadinessInvalidation != nil {
				t.Fatalf("malformed recheck changed item = %+v", got)
			}
			for _, id := range publicationScheduleIDs(item.ID) {
				if schedule := readScheduleRow(t, st, id); schedule.Status != domain.ScheduleArmed {
					t.Fatalf("malformed recheck concluded schedule %s: %s", id, schedule.Status)
				}
			}
			pulls, issues, completion := readCaptureState(t, st)
			if len(pulls) != 0 || len(issues) != 0 || completion != nil {
				t.Fatalf("malformed recheck persisted facts: pulls=%v issues=%v completion=%v",
					pulls, issues, completion)
			}

			malformed = false
			pullCalls = 0
			if failures, err = reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
				t.Fatalf("retry Reconcile = %v, %v", failures, err)
			}
			if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusResolved ||
				got.ItemVersion != item.ItemVersion+1 {
				t.Fatalf("valid retry did not conclude item = %+v", got)
			}
		})
	}
}

// TestActiveResourceReconcileInvalidationYieldsToConcurrentConclusion is the
// #496 race: a conclusion that serializes between the observation and the
// commit wins, because the commit re-reads the item and only supersedes a
// still-open one. The pull observer concludes the item as a side effect,
// standing in for the concurrent writer.
func TestActiveResourceReconcileInvalidationYieldsToConcurrentConclusion(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			// A decision concludes the item after the observation reads it open
			// but before the commit re-reads it.
			concluded := readActiveItem(t, st, item.ID)
			concluded.Status = domain.StatusResolved
			concluded.ItemVersion++
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, concluded)
			}); err != nil {
				t.Fatal(err)
			}
			observed := exactPull("open", false)
			observed.HeadSHA = "f00dbabe"
			return observed, nil
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	failures, err := reconciler.Reconcile(ctx)
	if err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusResolved || got.ReadinessInvalidation != nil {
		t.Fatalf("concurrent conclusion lost to invalidation = %+v", got)
	}
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

func TestActiveResourceReconcileRecoversSupersededCompletion(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	item.Status = domain.StatusSuperseded
	item.ItemVersion++
	item.BaseFreshness = &domain.BaseFreshness{
		BaseRef: "main", AdmittedBaseSHA: "cafebabe", ObservedBaseSHA: "deadbeef",
		Advanced: true, ObservedAt: activeResourceTestTime,
	}
	item.ReadinessInvalidation = &domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced,
		Bound:  "cafebabe", Observed: "deadbeef", ObservedAt: activeResourceTestTime,
	}
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
			return exactPull("closed", true), nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			return publish.IssueObservation{
				Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef",
			}, nil
		},
		now: func() time.Time { return activeResourceTestTime.Add(time.Minute) },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("recovery Reconcile = %v, %v", failures, err)
	}
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusSuperseded || got.ItemVersion != item.ItemVersion {
		t.Fatalf("completion recovery changed item = %+v", got)
	}
	pulls, issues, completion := readCaptureState(t, st)
	if len(pulls) != 1 || len(issues) != 1 || completion == nil {
		t.Fatalf("recovered capture = %v %v %v", pulls, issues, completion)
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("write-once Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 2 {
		t.Fatalf("completed unit was polled again: %d pull calls", pullCalls)
	}
	pulls, issues, completion = readCaptureState(t, st)
	if len(pulls) != 1 || len(issues) != 1 || completion == nil {
		t.Fatalf("write-once capture = %v %v %v", pulls, issues, completion)
	}
}

func TestActiveResourceReconcileFinallyEvictsAcrossBindingRecovery(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	runID := domain.RunID("run-binding-recovery")
	seedCaptureRun(t, st, runID)
	authority := seedReadyBindingAuthority(t, st, runID, "cafed00d")
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
	}, runID, "project-1", activeResourceTestTime.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-binding-recovery", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		PRHeadSHA:         "cafed00d", ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusResolved,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	pullCalls := 0
	evictions := 0
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			return exactPull("closed", true), nil
		},
		evictConcluded: func(domain.ReadyItemPRBinding, *int) {
			evictions++
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 1 {
		t.Fatalf("ready-unbound Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 0 || evictions != 0 {
		t.Fatalf("ready-unbound resource calls = pulls %d evictions %d, want 0 and 0", pullCalls, evictions)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, domain.ReadyItemPRBinding{
			ItemID: item.ID, RunID: runID,
			ProducingInvocationID: authority.invocationID, PublicationIdentity: authority.identity,
			PublicationInvocationID: authority.publicationInvocationID,
			Repo:                    "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: "cafed00d",
			RecordedAt: activeResourceTestTime,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("unit-unbound Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 0 || evictions != 1 {
		t.Fatalf("unit-unbound resource calls = pulls %d evictions %d, want 0 and 1", pullCalls, evictions)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordWorkUnitPRBinding(ctx, domain.WorkUnitPRBinding{
			UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: "cafed00d",
			RecordedAt: activeResourceTestTime,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("bound Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 1 || evictions != 2 {
		t.Fatalf("bound resource calls = pulls %d evictions %d, want 1 and 2", pullCalls, evictions)
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("completed Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 1 || evictions != 2 {
		t.Fatalf("completed resource calls = pulls %d evictions %d, want 1 and 2", pullCalls, evictions)
	}
}

func TestActiveResourceReconcileRecoversAfterClosedUnmergedPull(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	item.Status = domain.StatusDismissed
	item.ItemVersion++
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	closed := domain.PullMergeFact{
		Repo: "owner/repo", RepositoryID: 424242, PRNumber: 450,
		State: domain.PullRequestClosed, Merged: false,
		BaseRef: "main", HeadSHA: "cafed00d", ObservedAt: activeResourceTestTime,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AppendPullMergeFact(ctx, closed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	merged := false
	pullCalls := 0
	evictions := 0
	reconciler := activeResourceReconciler{
		store: st,
		pull: func(context.Context, string, int) (publish.PullObservation, error) {
			pullCalls++
			return exactPull("closed", merged), nil
		},
		issue: func(context.Context, string, int) (publish.IssueObservation, error) {
			return publish.IssueObservation{
				Number: 443, State: "closed", ClosedByCommitSHA: "deadbeef",
			}, nil
		},
		evictConcluded: func(domain.ReadyItemPRBinding, *int) {
			evictions++
		},
		now: func() time.Time { return activeResourceTestTime.Add(time.Minute) },
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("closed-unmerged Reconcile = %v, %v", failures, err)
	}
	if evictions != 1 {
		t.Fatalf("initial concluded eviction count = %d, want 1", evictions)
	}
	merged = true
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("reopened-and-merged Reconcile = %v, %v", failures, err)
	}
	pulls, issues, completion := readCaptureState(t, st)
	if len(pulls) != 2 || pulls[0].Merged || !pulls[1].Merged ||
		len(issues) != 1 || completion == nil {
		t.Fatalf("reopened capture = %v %v %v", pulls, issues, completion)
	}
	if evictions != 2 {
		t.Fatalf("post-completion eviction count = %d, want 2", evictions)
	}
	if failures, err := reconciler.Reconcile(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("completed Reconcile = %v, %v", failures, err)
	}
	if pullCalls != 3 || evictions != 2 {
		t.Fatalf("completed resource calls = pulls %d evictions %d, want 3 and 2", pullCalls, evictions)
	}
}
