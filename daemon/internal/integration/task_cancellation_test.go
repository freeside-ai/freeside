package integration_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestTaskCancellationStopsDurableFakeRuntimeAcrossRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "restarted-driver"}[restart], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
				driveApprovedCampaign(t, f)
				invocation := productionInvocationForRun(attemptSweepRunID)
				f.driver.Script(invocation, fake.StageScript{RunningInspects: 100, Outcome: fake.OutcomeComplete, Result: exec.StageResult{Summary: "late output"}})
				for range 3 {
					if _, err := f.engine.Reconcile(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				live, err := f.driver.Inspect(t.Context(), invocation)
				if err != nil || !live.Live {
					t.Fatalf("expected live fixture: %+v, %v", live, err)
				}
				var run domain.Run
				if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
					var err error
					run, err = tx.GetRun(t.Context(), attemptSweepRunID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				state, err := f.store.ServerState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				command := signet.ClientCommand{
					CommandID: "runtime-stop", DeviceID: deviceA, Kind: domain.CommandKindStopTask, ExpectedEntityVersion: state.Revision,
					StopTask: signet.StopTaskPayload{TaskID: run.TaskID, ProjectID: run.ProjectID, ExpectedSyncEpoch: state.SyncEpoch},
				}
				receipt, err := f.signet.Submit(t.Context(), command)
				if err != nil {
					t.Fatal(err)
				}
				driver := f.driver
				if restart {
					driver, err = fake.NewStageDriverAt(filepath.Join(f.root, "driver"))
					if err != nil {
						t.Fatal(err)
					}
				}
				coordinator, err := engine.New(f.store, f.signet, driver, engine.WithTaskCancellationRuntime(engine.TaskCancellationRuntime{
					Timeout: time.Second,
					StopRun: func(ctx context.Context, owned domain.Run, reviews []domain.ReviewRequestRecord) error {
						if len(reviews) != 0 {
							return errors.New("fixture unexpectedly owns review work")
						}
						for _, stage := range owned.Stages {
							for _, attempt := range stage.Attempts {
								if err := driver.Cancel(ctx, attempt.InvocationID); err != nil {
									return err
								}
								result, err := driver.Collect(ctx, attempt.InvocationID)
								if err != nil {
									return err
								}
								if result.Status != exec.StatusCanceled && result.Status != exec.StatusCompleted {
									return errors.New("fake runtime did not settle")
								}
							}
						}
						// This fake owns no external resources: its durable committed
						// terminal is its complete execution state, unlike ward.
						return nil
					},
				}))
				if err != nil {
					t.Fatal(err)
				}
				if err := coordinator.ReconcileTaskCancellations(t.Context()); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if err := coordinator.ReconcileTaskCancellations(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
					task, err := tx.GetTask(t.Context(), run.TaskID)
					want := domain.TaskCancellationConfirmed
					if restart {
						want = domain.TaskCancellationFailed
					}
					if err == nil && (task.Cancellation.State != want || domain.TaskWIP(task) != restart) {
						t.Fatalf("stopped task retained WIP or lacks confirmation: %+v", task)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				settled, err := driver.Collect(t.Context(), invocation)
				if restart {
					if !errors.Is(err, exec.ErrNoResult) {
						t.Fatalf("lost session invented a result: %+v, %v", settled, err)
					}
				} else if err != nil || settled.Status != exec.StatusCanceled {
					t.Fatalf("owned invocation: %+v, %v", settled, err)
				}
				replayed, err := f.signet.Submit(t.Context(), command)
				if err != nil || !reflect.DeepEqual(receipt, replayed) {
					t.Fatal("Stop receipt changed after confirmation")
				}
				before, _ := f.store.ServerState(t.Context())
				if err := coordinator.ReconcileTaskCancellations(t.Context()); err != nil {
					t.Fatal(err)
				}
				after, _ := f.store.ServerState(t.Context())
				if before != after {
					t.Fatal("repeated runtime proof changed revision")
				}
			})
		})
	}
}
