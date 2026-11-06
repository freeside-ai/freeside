package integration_test

import (
	"context"
	"errors"
	"fmt"
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

// TestTaskStopRetiresItsSpecificationFailureCard is #1526: stopping a live
// specification invocation must not leave an execution-failure card asking
// the operator to recover the task they deliberately ended, whichever of the
// engine's collection and the Stop's confirmation commits first.
func TestTaskStopRetiresItsSpecificationFailureCard(t *testing.T) {
	for _, collectFirst := range []bool{true, false} {
		name := map[bool]string{true: "collected-before-confirmation", false: "confirmed-before-collection"}[collectFirst]
		t.Run(name, func(t *testing.T) {
			// The bubble's clock starts before the fixture's fixed
			// specification clock, so the canceled outcome is stamped after
			// the Stop is requested, as it is in production.
			synctest.Test(t, func(t *testing.T) {
				f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
				_, specificationRunID := submitCampaignSpecification(t, f)
				invocation := domain.InvocationID(engine.SpecificationDispatchMarkerKey(specificationRunID))
				f.driver.Script(invocation, fake.StageScript{
					RunningInspects: 100, Outcome: fake.OutcomeComplete,
					Result: exec.StageResult{Artifacts: []domain.Digest{}, Summary: "late specification"},
				})
				if started := reconcileCapabilityRetryPass(t, f, "specification start"); started.InvocationsStarted != 1 {
					t.Fatalf("first pass = %+v, want the specification started", started)
				}
				if live, err := f.driver.Inspect(t.Context(), invocation); err != nil || !live.Live {
					t.Fatalf("expected a live specification: %+v, %v", live, err)
				}
				var run domain.Run
				if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
					var err error
					run, err = tx.GetRun(t.Context(), specificationRunID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				state, err := f.store.ServerState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
					CommandID: "specification-stop", DeviceID: deviceA, Kind: domain.CommandKindStopTask,
					ExpectedEntityVersion: state.Revision,
					StopTask:              signet.StopTaskPayload{TaskID: run.TaskID, ProjectID: run.ProjectID, ExpectedSyncEpoch: state.SyncEpoch},
				}); err != nil {
					t.Fatal(err)
				}
				coordinator, err := engine.New(f.store, f.signet, f.driver, engine.WithTaskCancellationRuntime(engine.TaskCancellationRuntime{
					Timeout: time.Second,
					StopRun: func(ctx context.Context, owned domain.Run, _ []domain.ReviewRequestRecord) error {
						// The specification invocation is the task's only
						// work, and the fake owns nothing beyond it.
						if owned.ID != specificationRunID {
							return nil
						}
						return f.driver.Cancel(ctx, invocation)
					},
				}))
				if err != nil {
					t.Fatal(err)
				}
				confirm := func() {
					t.Helper()
					for range 2 {
						if err := coordinator.ReconcileTaskCancellations(t.Context()); err != nil {
							t.Fatal(err)
						}
						synctest.Wait()
					}
				}
				cardID := domain.ItemID("execution-failure-" + string(invocation))
				if collectFirst {
					// The runtime's cancel lands, and the engine collects
					// it, before the Stop's proof is acknowledged.
					if err := f.driver.Cancel(t.Context(), invocation); err != nil {
						t.Fatal(err)
					}
					reconcileCapabilityRetryPass(t, f, "collect canceled specification")
					card, err := f.signet.GetAttentionItem(t.Context(), cardID)
					if err != nil || card.Item.Status != domain.StatusOpen {
						t.Fatalf("canceled specification card before confirmation: %+v, %v", card.Item.Status, err)
					}
					confirm()
					// The confirming write retires it, before any sweep.
					if card, err := f.signet.GetAttentionItem(t.Context(), cardID); err != nil || card.Item.Status != domain.StatusExpired {
						t.Fatalf("card after confirmation = %+v, %v; want expired", card.Item.Status, err)
					}
				} else {
					confirm()
					if _, err := f.signet.GetAttentionItem(t.Context(), cardID); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("card raised before collection: %v", err)
					}
					// The pass that collects the result raises the card and
					// expires it, so no pass leaves it open for delivery.
					reconcileCapabilityRetryPass(t, f, "collect after confirmation")
					if card, err := f.signet.GetAttentionItem(t.Context(), cardID); err != nil || card.Item.Status != domain.StatusExpired {
						t.Fatalf("card after the collecting pass = %+v, %v; want expired", card.Item.Status, err)
					}
				}
				for pass := range 3 {
					if result := reconcileCapabilityRetryPass(t, f, "after stop"); result.InvocationsStarted != 0 {
						t.Fatalf("pass %d after Stop started work: %+v", pass, result)
					}
				}
				if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
					task, err := tx.GetTask(t.Context(), run.TaskID)
					if err != nil {
						return err
					}
					if task.Cancellation == nil || task.Cancellation.State != domain.TaskCancellationConfirmed || domain.TaskWIP(task) {
						t.Fatalf("stopped task lost its fence or kept WIP: %+v", task.Cancellation)
					}
					item, err := tx.GetAttentionItem(t.Context(), cardID)
					if err != nil {
						return err
					}
					if item.Status != domain.StatusExpired || item.Conclusion == nil ||
						item.Conclusion.Cause != domain.ConcludedByExpiry {
						t.Fatalf("card after confirmed Stop = %s, conclusion %+v", item.Status, item.Conclusion)
					}
					open, err := tx.ListOpenAttentionItemRecordsForTask(t.Context(), run.TaskID)
					if err != nil {
						return err
					}
					for _, other := range open {
						if other.Type == domain.AttentionExecutionFailure {
							t.Fatalf("stopped task still has an open execution failure: %s", other.ID)
						}
					}
					if _, err := tx.GetInbox(t.Context(), string(invocation)); err != nil {
						return fmt.Errorf("specification terminal: %w", err)
					}
					outcome, err := tx.GetExecutionOutcomeRecord(t.Context(), invocation)
					if err != nil || outcome.Status != domain.ExecutionOutcomeCanceled {
						t.Fatalf("execution outcome = %+v, %v; want canceled history", outcome, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
