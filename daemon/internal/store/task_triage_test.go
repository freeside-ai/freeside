package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var triageRecordedAt = time.Date(2026, 11, 3, 12, 0, 0, 7, time.UTC)

func recordTaskTriage(ctx context.Context, s *store.Store, record domain.TaskTriage) error {
	return s.Write(ctx, func(tx *store.WriteTx) error { return tx.RecordTaskTriage(ctx, record) })
}

func readTaskTriage(ctx context.Context, s *store.Store, taskID domain.TaskID) (domain.TaskTriage, bool, error) {
	var (
		record domain.TaskTriage
		found  bool
	)
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, found, err = tx.GetTaskTriage(ctx, taskID)
		return err
	})
	return record, found, err
}

// Each outcome round-trips through the store, with its tier and reason when
// it has them.
func TestTaskTriageRoundTripsEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range domain.AllTriageOutcomes {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := openStore(t, store.Options{})
			taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
			if _, found, err := readTaskTriage(ctx, s, taskID); err != nil || found {
				t.Fatalf("triage before any record = %t, %v", found, err)
			}
			record := domain.TaskTriage{TaskID: taskID, Outcome: outcome, Escalated: true, RecordedAt: triageRecordedAt}
			if outcome == domain.TriageOutcomeAnswered {
				record.Tier, record.Escalated = new(domain.TriageTierStandard), false
				record.Reason = new("A one-file docs fix.")
			}
			if err := recordTaskTriage(ctx, s, record); err != nil {
				t.Fatal(err)
			}
			got, found, err := readTaskTriage(ctx, s, taskID)
			if err != nil || !found || !reflect.DeepEqual(got, record) {
				t.Fatalf("triage = %+v, %t, %v; want %+v", got, found, err, record)
			}
		})
	}
}

// A task holds one triage record: a second is refused and the first stays.
func TestTaskTriageRefusesASecondRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	first := domain.TaskTriage{
		TaskID: taskID, Outcome: domain.TriageOutcomeCallFailed, Escalated: true, RecordedAt: triageRecordedAt,
	}
	if err := recordTaskTriage(ctx, s, first); err != nil {
		t.Fatal(err)
	}
	second := domain.TaskTriage{
		TaskID: taskID, Outcome: domain.TriageOutcomeAnswered, Tier: new(domain.TriageTierStandard),
		RecordedAt: triageRecordedAt.Add(time.Minute),
	}
	if err := recordTaskTriage(ctx, s, second); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("second record = %v, want %v", err, domain.ErrDuplicate)
	}
	if got, _, err := readTaskTriage(ctx, s, taskID); err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("triage after the refusal = %+v, %v; want %+v", got, err, first)
	}
}

// An invalid record and a record for no task write nothing.
func TestTaskTriageRefusesBadRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	quiet := domain.TaskTriage{TaskID: taskID, Outcome: domain.TriageOutcomeBudgetSpent, RecordedAt: triageRecordedAt}
	if err := recordTaskTriage(ctx, s, quiet); !errors.Is(err, domain.ErrTaskTriageInconsistent) {
		t.Fatalf("a skip that did not escalate = %v", err)
	}
	orphan := domain.TaskTriage{
		TaskID: "no-such-task", Outcome: domain.TriageOutcomeBudgetSpent, Escalated: true, RecordedAt: triageRecordedAt,
	}
	if err := recordTaskTriage(ctx, s, orphan); err == nil {
		t.Fatal("a record for no task was stored")
	}
	if _, found, err := readTaskTriage(ctx, s, taskID); err != nil || found {
		t.Fatalf("triage after refusals = %t, %v", found, err)
	}
}
