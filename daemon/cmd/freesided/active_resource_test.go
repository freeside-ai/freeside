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

func armActiveTestSchedules(t *testing.T, st *store.Store, item domain.AttentionItem) {
	t.Helper()
	ctx := context.Background()
	itemID, version := item.ID, item.ItemVersion
	runID := *item.Subject.RunID
	policy := schedTestResolvedPolicy(runID)
	deadline := activeResourceTestTime.Add(time.Hour)
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
			if err := tx.SetScheduleTimer(ctx, schedule.ID, schedule.Generation, *schedule.FireAt); err != nil {
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
