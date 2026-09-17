package signet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestStoppedQueuedTaskWireGolden(t *testing.T) {
	at := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	target := domain.TaskCancellationTarget{TaskID: "task-queued", ProjectID: "project", Runs: []domain.TaskCancellationRun{}}
	digest := target.Digest("epoch")
	c := domain.TaskCancellation{
		RequestID: "cancel-queued", Target: target, TargetDigest: digest, SyncEpoch: "epoch",
		FenceRevision: 2, RequestedAt: at, State: domain.TaskCancellationConfirmed,
		Acknowledgement: &domain.TaskCancellationAcknowledgement{ID: "ack-queued", RequestID: "cancel-queued", TargetDigest: digest, State: domain.TaskCancellationConfirmed, EvidenceDigest: digest, RecordedAt: at},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	task := domain.Task{ID: target.TaskID, ProjectID: target.ProjectID, CreatedAt: at, CampaignIDs: []domain.CampaignID{}, Name: domain.DisplayName{Text: "Queued work", Source: domain.DisplayNameSourceOperator}, Cancellation: &c}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot := TaskSnapshot{EntityVersion: 3, AsOfRevision: 3, Task: Task{
		ID: task.ID, ProjectID: task.ProjectID, CreatedAt: at, LastActivityAt: at,
		DisplayNames: domain.DisplayNames{Task: task.Name, Project: domain.DisplayName{Text: "project", Source: domain.DisplayNameSourceIdentifier}},
		Lifecycle:    task.DisplayLifecycle(nil), CampaignIDs: task.CampaignIDs, RunIDs: []domain.RunID{}, WIP: domain.TaskWIP(task), LifecycleFacts: []TaskLifecycleFact{}, Cancellation: &c,
	}}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "task-stopped-queued", append(data, '\n'))
}
