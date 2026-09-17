package signet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestStopTaskWireGoldens(t *testing.T) {
	target := domain.TaskCancellationTarget{TaskID: "task-1", ProjectID: "project-1", Runs: []domain.TaskCancellationRun{}}
	receipt := domain.StopTaskReceipt{StopTaskRequest: domain.StopTaskRequest{CommandID: "stop-1", DeviceID: "device-1", TaskID: target.TaskID, ProjectID: target.ProjectID, ExpectedSyncEpoch: "epoch-1", ExpectedEntityVersion: 4}, Cancellation: domain.TaskCancellation{RequestID: "cancel-1", Target: target, TargetDigest: target.Digest("epoch-1"), SyncEpoch: "epoch-1", FenceRevision: 5, RequestedAt: time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC), State: domain.TaskCancellationRequested}}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	result := normalizeCommandResult(CommandResult{Stop: &receipt, Revision: 5})
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "stop-task-result", b)
}
