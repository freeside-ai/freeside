package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestRunHoldImageUpkeepRoundTripsAndFailsClosed: each image_upkeep_pending
// arm reads back exactly, and a row whose upkeep columns the contract cannot
// express fails the read instead of dropping the fact.
func TestRunHoldImageUpkeepRoundTripsAndFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/run-hold-upkeep.db", Options{})
	run := domain.Run{ID: "run-1", ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	refusal := domain.Digest("sha256:" + strings.Repeat("e1", 32))
	item, task := domain.ItemID("item-adopt"), domain.TaskID("task-maintain")
	getHold := func() (domain.RunHoldObservation, error) {
		var hold domain.RunHoldObservation
		err := st.Read(ctx, func(tx *ReadTx) error {
			var (
				found bool
				err   error
			)
			hold, found, err = tx.GetRunHold(ctx, run.ID)
			if err == nil && !found {
				t.Fatal("hold missing")
			}
			return err
		})
		return hold, err
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	for _, fact := range []domain.ImageUpkeepHold{
		{Pending: domain.ImageUpkeepPendingRebuild},
		{Pending: domain.ImageUpkeepPendingAdoptionProposal, RefusalRecordID: &refusal, ProposalItemID: &item},
		{Pending: domain.ImageUpkeepPendingMaintenanceTask, RefusalRecordID: &refusal, MaintenanceTaskID: &task},
	} {
		want := domain.RunHoldObservation{
			RunID: run.ID, Reason: domain.HoldImageUpkeepPending, ImageUpkeep: &fact,
			FirstObservedAt: at, LastObservedAt: at,
		}
		if err := st.Write(ctx, func(tx *WriteTx) error { return tx.RecordRunHold(ctx, want) }); err != nil {
			t.Fatalf("%s: record: %v", fact.Pending, err)
		}
		got, err := getHold()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: read = %+v err=%v, want %+v", fact.Pending, got, err, want)
		}
	}

	for _, tc := range []struct {
		name, update string
		want         error
	}{
		{
			"unknown pending", `UPDATE run_hold_observations SET upkeep_pending = 'vibes'`,
			domain.ErrInvalidImageUpkeepPending,
		},
		{
			"task without a pending kind", `UPDATE run_hold_observations SET upkeep_pending = NULL`,
			domain.ErrHoldDetailMismatch,
		},
		{"reason without its fact", `UPDATE run_hold_observations
			SET upkeep_refusal_id = NULL, upkeep_task_id = NULL`, domain.ErrHoldDetailMismatch},
		{"fact beside another reason", `UPDATE run_hold_observations
			SET reason = 'operation_stopped', upkeep_pending = 'rebuild'`, domain.ErrHoldDetailMismatch},
	} {
		if _, err := st.db.ExecContext(ctx, tc.update); err != nil {
			t.Fatal(err)
		}
		if _, err := getHold(); !errors.Is(err, tc.want) {
			t.Errorf("%s: GetRunHold() = %v, want %v", tc.name, err, tc.want)
		}
	}
}
