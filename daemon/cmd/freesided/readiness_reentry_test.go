package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var (
	reentryTestBase     = strings.Repeat("b", 40)
	reentryTestHead     = strings.Repeat("a", 40)
	reentryTestNewBase  = strings.Repeat("c", 40)
	reentryTestNewHead  = strings.Repeat("d", 40)
	reentryTestReviewID = domain.InvocationID("review-reentry-round-1")
)

// reentryReadyItem seeds what a published production run leaves behind and the
// re-entry authority's gate reads: the run's production ready item, its pull
// request binding, and a clean review of the bound head.
func reentryReadyItem(t *testing.T, st *store.Store, runID domain.RunID) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	seedCaptureRun(t, st, runID)
	authority := seedReadyBindingAuthority(t, st, runID, reentryTestHead)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ProductionReadyItemID(runID), ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		PRHeadSHA:         reentryTestHead,
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: reentryTestReviewID, RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: reentryTestBase, HeadSHA: reentryTestHead,
		CompletedAt:        activeResourceTestTime.Add(-2 * time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, review, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, domain.ReadyItemPRBinding{
			ItemID: item.ID, RunID: runID,
			ProducingInvocationID: authority.invocationID, PublicationIdentity: authority.identity,
			PublicationInvocationID: authority.publicationInvocationID,
			Repo:                    "owner/repo", RepositoryID: 424242,
			PRNumber: 450, BaseRef: "main", HeadSHA: reentryTestHead,
			RecordedAt: activeResourceTestTime.Add(-time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// readReentry returns the run's current re-entry authority and whether the
// production lane has a pending task under that authority's key.
func readReentry(t *testing.T, st *store.Store, runID domain.RunID) (*domain.PublicationSuccessor, bool) {
	t.Helper()
	ctx := context.Background()
	var (
		successor *domain.PublicationSuccessor
		queued    bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if successor, err = tx.CurrentPublicationSuccessor(ctx, runID); err != nil {
			return err
		}
		pending, err := tx.ListPendingOutbox(ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if successor == nil {
			queued = len(pending) != 0
			return nil
		}
		for _, entry := range pending {
			if entry.IdempotencyKey == successor.TaskKey() {
				queued = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return successor, queued
}

func assertReentryStarted(
	t *testing.T, st *store.Store, item domain.AttentionItem,
	reason domain.ReadinessInvalidationReason, base, head string,
) {
	t.Helper()
	successor, queued := readReentry(t, st, *item.Subject.RunID)
	if successor == nil || !queued {
		t.Fatalf("re-entry = authority %+v, task queued %t; want both", successor, queued)
	}
	if successor.Origin != domain.PublicationSuccessorReadinessInvalidation ||
		successor.PredecessorItemID != item.ID ||
		successor.PriorReviewInvocationID != reentryTestReviewID || successor.ReviewRound != 2 ||
		successor.Reentry == nil || successor.Reentry.Reason != reason ||
		successor.Reentry.BaseSHA != base || successor.Reentry.HeadSHA != head {
		t.Fatalf("re-entry authority = %+v (re-entry %+v)", successor, successor.Reentry)
	}
}

func assertNoReentry(t *testing.T, st *store.Store, runID domain.RunID) {
	t.Helper()
	if successor, queued := readReentry(t, st, runID); successor != nil || queued {
		t.Fatalf("re-entry = authority %+v, task queued %t; want neither", successor, queued)
	}
}

// TestBaseAdvanceWatchStartsReadinessReentry proves the base watch records the
// re-entry authority and its task in the transaction that supersedes the ready
// item, with the observed tip as the cycle's base and the bound head kept
// (issue #502).
func TestBaseAdvanceWatchStartsReadinessReentry(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-reentry-base")
	sched := watchSchedule(t, item)
	now := sched.CreatedAt
	s, err := scheduler.New(st, domain.ModeAttendedDev,
		func() time.Time { return now },
		map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleBaseAdvanceWatch: baseAdvanceRegistration(st,
				func(context.Context, domain.ScheduleBaseWatch) (string, error) {
					return reentryTestNewBase, nil
				}, mergeCapture{}),
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Arm(ctx, sched, sched.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	now = sched.CreatedAt.Add(61 * time.Second)
	if err := s.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusSuperseded || got.ReadinessInvalidation == nil ||
		got.ReadinessInvalidation.Reason != domain.ReadinessInvalidationBaseAdvanced {
		t.Fatalf("advanced item = status %s invalidation %+v", got.Status, got.ReadinessInvalidation)
	}
	assertReentryStarted(t, st, item,
		domain.ReadinessInvalidationBaseAdvanced, reentryTestNewBase, reentryTestHead)
}

// TestActiveResourceReconcileStartsReadinessReentry proves the reconciler
// starts a cycle for a pushed head, reviewed against the prior review's base,
// and starts none for an invalidation no cycle can answer (issue #502).
func TestActiveResourceReconcileStartsReadinessReentry(t *testing.T) {
	reconcile := func(t *testing.T, st *store.Store, mutate func(*publish.PullObservation)) {
		t.Helper()
		reconciler := activeResourceReconciler{
			store: st,
			pull: func(context.Context, string, int) (publish.PullObservation, error) {
				observed := exactPull("open", false)
				observed.HeadSHA = reentryTestHead
				mutate(&observed)
				return observed, nil
			},
			now: func() time.Time { return activeResourceTestTime },
		}
		if failures, err := reconcileActiveResource(&reconciler, context.Background()); err != nil || len(failures) != 0 {
			t.Fatalf("Reconcile = %v, %v", failures, err)
		}
	}

	t.Run("head changed", func(t *testing.T) {
		st := schedTestStore(t)
		item := reentryReadyItem(t, st, "run-reentry-head")
		push := func(p *publish.PullObservation) { p.HeadSHA = reentryTestNewHead }
		reconcile(t, st, push)
		assertReentryStarted(t, st, item,
			domain.ReadinessInvalidationHeadChanged, reentryTestBase, reentryTestNewHead)

		// A second pass finds the item superseded and the cycle already
		// recorded: it neither fails nor starts another.
		reconcile(t, st, push)
		assertReentryStarted(t, st, item,
			domain.ReadinessInvalidationHeadChanged, reentryTestBase, reentryTestNewHead)
	})

	for _, tc := range []struct {
		name   string
		mutate func(*publish.PullObservation)
		reason domain.ReadinessInvalidationReason
	}{
		{
			name:   "retargeted",
			mutate: func(p *publish.PullObservation) { p.BaseRef = "release" },
			reason: domain.ReadinessInvalidationRetargeted,
		},
		{
			name:   "identity changed",
			mutate: func(p *publish.PullObservation) { p.BaseRepoID = 434343 },
			reason: domain.ReadinessInvalidationIdentityChanged,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := schedTestStore(t)
			item := reentryReadyItem(t, st, "run-reentry-none")
			reconcile(t, st, tc.mutate)
			got := readActiveItem(t, st, item.ID)
			if got.Status != domain.StatusSuperseded || got.ReadinessInvalidation == nil ||
				got.ReadinessInvalidation.Reason != tc.reason {
				t.Fatalf("item = status %s invalidation %+v", got.Status, got.ReadinessInvalidation)
			}
			assertNoReentry(t, st, *item.Subject.RunID)
		})
	}
}
