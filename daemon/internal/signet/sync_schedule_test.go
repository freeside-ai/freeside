package signet_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestScheduleSyncSurface: §5.16 "schedule state is durable, queryable, and
// synced" at the service boundary — a persisted schedule rides both the
// bootstrap snapshot and the partial /schedules fetch with its store-stamped
// metadata, and an empty collection encodes as [], never null.
func TestScheduleSyncSurface(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	empty, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if empty.Schedules == nil || len(empty.Schedules) != 0 {
		t.Fatalf("empty bootstrap schedules = %#v, want []", empty.Schedules)
	}

	itemID := f.item.ID
	version := f.item.ItemVersion
	fireAt := time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC)
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID:        "schedule-pr_checks_deadline-item-1",
		ProjectID: f.item.ProjectID, Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		CreatedAt: fireAt.Add(-30 * time.Minute), FireAt: &fireAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutSchedule(ctx, schedule)
	}); err != nil {
		t.Fatal(err)
	}

	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(bootstrap.Schedules) != 1 || bootstrap.Schedules[0].Schedule.ID != schedule.ID ||
		bootstrap.Schedules[0].EntityVersion != 1 ||
		bootstrap.Schedules[0].AsOfRevision > bootstrap.Revision {
		t.Fatalf("bootstrap schedules = %+v", bootstrap.Schedules)
	}

	listed, err := f.service.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	if len(listed) != 1 || listed[0].Schedule.ID != schedule.ID ||
		listed[0].Schedule.Kind != domain.SchedulePRChecksDeadline {
		t.Fatalf("listed schedules = %+v", listed)
	}
}
