package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// workUnitCompletedMilestones returns the run's work_unit_completed
// milestones as the observation reader reconstructs them.
func workUnitCompletedMilestones(t *testing.T, st *store.Store, runID domain.RunID) []domain.RunMilestone {
	t.Helper()
	var out []domain.RunMilestone
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		observation, err := tx.ObserveRun(context.Background(), runID)
		if err != nil {
			return err
		}
		for _, milestone := range observation.Milestones {
			if milestone.Kind == domain.MilestoneWorkUnitCompleted {
				out = append(out, milestone)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("observe %s: %v", runID, err)
	}
	return out
}

// assertCompletionMilestone pins the completion transaction's observation
// mirror: exactly one work_unit_completed milestone, on the run's publication
// invocation, at the completion record's instant.
func assertCompletionMilestone(t *testing.T, st *store.Store, runID domain.RunID, recordedAt time.Time) {
	t.Helper()
	milestones := workUnitCompletedMilestones(t, st, runID)
	if len(milestones) != 1 {
		t.Fatalf("run %s work_unit_completed milestones = %d, want 1", runID, len(milestones))
	}
	got := milestones[0]
	if got.InvocationID == nil || *got.InvocationID != domain.ProductionPublicationInvocationID(runID) ||
		!got.RecordedAt.Equal(recordedAt) {
		t.Fatalf("run %s milestone = %+v, want publication invocation at %s", runID, got, recordedAt)
	}
}

func storeRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var revision int64
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		state, err := tx.ServerState(context.Background())
		if err != nil {
			return err
		}
		revision = state.Revision
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return revision
}

// TestReconcileWorkUnitCompletionMilestones (#1134): a completion recorded
// without its milestone (a pre-0066 store) gains exactly one at the record's
// instant on start-up; a store already carrying it is a no-op that bumps no
// sync revision.
func TestReconcileWorkUnitCompletionMilestones(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	runID := *item.Subject.RunID
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
	var completion domain.WorkUnitCompletion
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
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
		var ok bool
		completion, ok = domain.EvaluateWorkUnitCompletion(declaration, binding, pull, &issue)
		if !ok {
			return errors.New("fixture did not derive completion")
		}
		return tx.RecordWorkUnitCompletion(ctx, completion)
	}); err != nil {
		t.Fatal(err)
	}
	if got := workUnitCompletedMilestones(t, st, runID); len(got) != 0 {
		t.Fatalf("milestones before reconcile = %v, want none", got)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := reconcileWorkUnitCompletionMilestones(ctx, st, logger); err != nil {
		t.Fatal(err)
	}
	assertCompletionMilestone(t, st, runID, completion.RecordedAt)

	revision := storeRevision(t, st)
	if err := reconcileWorkUnitCompletionMilestones(ctx, st, logger); err != nil {
		t.Fatal(err)
	}
	assertCompletionMilestone(t, st, runID, completion.RecordedAt)
	if got := storeRevision(t, st); got != revision {
		t.Fatalf("no-op reconcile moved the revision %d -> %d", revision, got)
	}
}

// TestReconcileWorkUnitCompletionMilestonesSkipsWithoutStandingReady: a
// completion whose run recorded a definitive block after its ready is not
// mirrored, because the sync boundary would refuse the milestone and an
// append-only milestone it refuses would exclude the run for good.
func TestReconcileWorkUnitCompletionMilestonesSkipsWithoutStandingReady(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	runID := *item.Subject.RunID
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
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
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
		invocation := domain.ProductionPublicationInvocationID(runID)
		reason := domain.HoldBaseAdvanced
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestonePublicationBlocked, InvocationID: &invocation,
			Reason: &reason, RecordedAt: activeResourceTestTime,
		})
	}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	revision := storeRevision(t, st)
	if err := reconcileWorkUnitCompletionMilestones(ctx, st, logger); err != nil {
		t.Fatal(err)
	}
	if got := workUnitCompletedMilestones(t, st, runID); len(got) != 0 {
		t.Fatalf("milestones after a block following ready = %v, want none", got)
	}
	if got := storeRevision(t, st); got != revision {
		t.Fatalf("skipped reconcile moved the revision %d -> %d", revision, got)
	}
}
