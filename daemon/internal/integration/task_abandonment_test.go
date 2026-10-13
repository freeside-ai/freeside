package integration_test

import (
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestAbandonedTaskReconcileAfterRetirement(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "approval"
		if failure {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
			var runID domain.RunID
			var itemID domain.ItemID
			if failure {
				driveApprovedCampaign(t, f)
				runID = attemptSweepRunID
				itemID = driveRunToFailureCard(t, f, runID)
			} else {
				_, specRun, approval := driveCampaignToApproval(t, f)
				runID, itemID = specRun, approval.Item.ID
			}
			var taskID domain.TaskID
			if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
				run, err := tx.GetRun(ctx, runID)
				if err != nil {
					return err
				}
				taskID = run.TaskID
				_, err = tx.AbandonTask(ctx, taskID, time.Now().UTC())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if item := readCardRecord(t, f, itemID); item.Status != domain.StatusOpen {
				t.Fatal("AbandonTask alone retired the item")
			}
			if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
				retired, err := engine.RetireAbandonedTaskItems(ctx, tx, taskID)
				if err == nil && !slices.Contains(retired, itemID) {
					t.Fatalf("retired = %v, missing %s", retired, itemID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if item := readCardRecord(t, f, itemID); item.Status != domain.StatusExpired {
				t.Fatalf("item status = %s", item.Status)
			}
			result := reconcileCapabilityRetryPass(t, f, "after abandonment")
			if result.InvocationsStarted != 0 {
				t.Fatalf("reconcile started work: %+v", result)
			}
		})
	}
}
