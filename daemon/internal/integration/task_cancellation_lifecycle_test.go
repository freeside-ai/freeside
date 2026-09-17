package integration_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func confirmFixtureTask(t *testing.T, st *store.Store, taskID domain.TaskID) {
	t.Helper()
	ctx := t.Context()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		receipt, _, err := tx.StopTask(ctx, domain.StopTaskRequest{CommandID: "fixture-stop", DeviceID: "fixture-device", TaskID: taskID, ProjectID: task.ProjectID, ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision}, time.Now().UTC())
		if err != nil {
			return err
		}
		c := receipt.Cancellation
		_, err = tx.AcknowledgeTaskCancellation(ctx, domain.TaskCancellationAcknowledgement{ID: "fixture-ack", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationConfirmed, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSpecificationApprovalStopNeedsConfirmedTaskAcknowledgement(t *testing.T) {
	f := openWideCapabilityRetryFixture(t, t.TempDir(), true)
	_, specRunID, approval := driveCampaignToApproval(t, f)
	if len(approval.Item.ArtifactDigests) == 0 {
		t.Fatal("fixture has no approved specification artifact")
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "legacy-stop", DeviceID: deviceA, ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{ItemID: approval.Item.ID, Action: domain.ActionStop, ItemVersion: approval.Item.ItemVersion, ArtifactDigests: approval.Item.ArtifactDigests},
	}); err != nil {
		t.Fatal(err)
	}
	var taskID domain.TaskID
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), specRunID)
		if err != nil {
			return err
		}
		taskID = run.TaskID
		task, err := tx.GetTask(t.Context(), taskID)
		if err != nil {
			return err
		}
		if !domain.TaskWIP(task) || task.Cancellation == nil || task.Cancellation.State != domain.TaskCancellationRequested {
			t.Fatal("legacy card Stop certified quiescence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	confirmFixtureTask(t, f.store, taskID)
	bootstrap, err := f.signet.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range bootstrap.Tasks {
		if s.Task.ID == taskID && (s.Task.WIP || s.Task.Lifecycle == nil || *s.Task.Lifecycle != domain.TaskStopped) {
			t.Fatalf("confirmed task: %+v", s.Task)
		}
	}
	retained, err := f.signet.GetAttentionItem(t.Context(), approval.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Item.Status != domain.StatusResolved || !reflect.DeepEqual(retained.Item.ArtifactDigests, approval.Item.ArtifactDigests) {
		t.Fatal("acknowledgement changed specification approval evidence")
	}
}

func TestConfirmedCancellationPreservesPublishedPRAndRunHistory(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)
	before, err := p.attention.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var taskID domain.TaskID
	for _, s := range before.Runs {
		if s.Run.ID == p.runID {
			taskID = s.Run.TaskID
		}
	}
	if taskID == "" {
		t.Fatal("published task absent")
	}
	readyBefore, err := p.attention.GetAttentionItem(t.Context(), domain.ProductionReadyItemID(p.runID))
	if err != nil {
		t.Fatal(err)
	}
	confirmFixtureTask(t, p.store, taskID)
	after, err := p.attention.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range before.Runs {
		if !reflect.DeepEqual(before.Runs[i].Run, after.Runs[i].Run) {
			t.Fatal("confirmation rewrote run history")
		}
	}
	readyAfter, err := p.attention.GetAttentionItem(t.Context(), readyBefore.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(readyBefore.Item, readyAfter.Item) || readyAfter.Item.PRReference == nil {
		t.Fatal("confirmation changed published PR or approval")
	}
	for _, s := range after.Tasks {
		if s.Task.ID == taskID && (s.Task.WIP || s.Task.Lifecycle == nil || *s.Task.Lifecycle != domain.TaskStopped) {
			t.Fatalf("published stopped task: %+v", s.Task)
		}
	}
}
