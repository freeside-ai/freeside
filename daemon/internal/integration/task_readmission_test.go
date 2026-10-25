package integration_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/intake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestReattemptTaskAdmissionIsAtomicWithAllocation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, cap               string
		abandon, occupy, cancel bool
		closed, live            bool
		want                    error
	}{
		{"held retry needs no new slot", "", false, false, false, false, false, nil},
		{"released retry admitted", "1", true, false, false, false, false, nil},
		{"released slot taken", "1", true, true, false, false, false, store.ErrTaskWIPCapExhausted},
		{"missing cap", "", true, false, false, false, false, intake.ErrIntakePolicyMissing},
		{"malformed cap", "unlimited", true, false, false, false, false, intake.ErrIntakePolicyMalformed},
		{"pending cancellation", "1", false, false, true, false, false, store.ErrTaskCancellationFenced},
		{"closed live parent admitted", "1", true, false, false, true, true, nil},
		{"closed live parent cap refusal", "1", true, true, false, true, true, store.ErrTaskWIPCapExhausted},
		{"closed parent still cancelled", "1", true, false, true, true, true, store.ErrTaskCancellationFenced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
			var keys []domain.PolicyKey
			if tc.cap != "" {
				keys = []domain.PolicyKey{{Key: intake.PolicyRunWIPCap, Value: tc.cap, Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: submissionDigest("readmission", "policy")}}}
			}
			campaign, _ := driveApprovedCampaign(t, f, keys...)
			if !tc.live {
				driveRunToFailureCard(t, f, attemptSweepRunID)
			} else {
				reconcileCapabilityRetryPass(t, f, "admit approved implementation")
			}
			var taskID domain.TaskID
			if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
				run, err := tx.GetRun(t.Context(), attemptSweepRunID)
				if err != nil {
					return err
				}
				taskID = run.TaskID
				if tc.abandon {
					if _, err := tx.AbandonTask(t.Context(), taskID, time.Now().UTC()); err != nil {
						return err
					}
				}
				if tc.closed {
					task, err := tx.GetTask(t.Context(), taskID)
					if err != nil {
						return err
					}
					if _, _, err := tx.CloseTaskWorkflow(t.Context(), taskID, task.CurrentStart().Ordinal, time.Now().UTC()); err != nil {
						return err
					}
				}
				if tc.occupy {
					other := domain.Run{ID: "other", ProjectID: run.ProjectID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}
					if err := tx.AssignTask(t.Context(), &other, nil); err != nil {
						return err
					}
					if err := tx.PutRun(t.Context(), other); err != nil {
						return err
					}
					if err := tx.AdmitTaskStart(t.Context(), other.ID, time.Now().UTC(), 1); err != nil {
						return err
					}
				}
				if tc.cancel {
					state, err := tx.ServerState(t.Context())
					if err != nil {
						return err
					}
					_, _, err = tx.StopTask(t.Context(), domain.StopTaskRequest{CommandID: "stop", DeviceID: "device", TaskID: taskID, ProjectID: run.ProjectID, ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision}, time.Now().UTC())
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, _ := f.store.ServerState(t.Context())
			retry, err := engine.ReattemptProductionRun(t.Context(), f.store, engine.ProductionReattemptSpec{ParentRunID: attemptSweepRunID, Reason: "Explicit retry"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("reattempt: %v, want %v", err, tc.want)
			}
			after, _ := f.store.ServerState(t.Context())
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				latest, err := tx.LatestProductionAttempt(t.Context(), campaign)
				if err != nil {
					return err
				}
				task, err := tx.GetTask(t.Context(), taskID)
				if err != nil {
					return err
				}
				if tc.want != nil {
					if latest.AttemptNumber != 1 || before != after {
						t.Fatal("rejected retry persisted allocation or revision")
					}
					if domain.TaskWIP(task) == tc.abandon {
						t.Fatal("refusal changed slot")
					}
				} else {
					if latest.AttemptNumber != 2 || !domain.TaskWIP(task) || after.Revision != before.Revision+1 {
						t.Fatal("allocation, admission and submission were not one write")
					}
					if tc.abandon && task.CurrentStart().RunID != retry.Run.Run.ID {
						t.Fatal("readmission not bound to new run")
					}
					if tc.closed {
						if err := tx.RequireRunWorkflowOpen(t.Context(), attemptSweepRunID); !errors.Is(err, store.ErrTaskWorkflowClosed) {
							t.Fatalf("new admission changed old fence: %v", err)
						}
						if err := tx.RequireRunWorkflowOpen(t.Context(), retry.Run.Run.ID); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
