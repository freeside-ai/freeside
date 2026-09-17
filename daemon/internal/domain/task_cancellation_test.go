package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestTaskCancellationGolden(t *testing.T) {
	target := domain.TaskCancellationTarget{TaskID: "task-1", ProjectID: "project-1", Runs: []domain.TaskCancellationRun{{RunID: "run-1"}}}
	c := domain.TaskCancellation{RequestID: "cancel-1", Target: target, TargetDigest: target.Digest("epoch-1"), SyncEpoch: "epoch-1", FenceRevision: 5, RequestedAt: time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC), State: domain.TaskCancellationRequested}
	r := domain.StopTaskReceipt{StopTaskRequest: domain.StopTaskRequest{CommandID: "stop-1", DeviceID: "device-1", TaskID: "task-1", ProjectID: "project-1", ExpectedSyncEpoch: "epoch-1", ExpectedEntityVersion: 4}, Cancellation: c}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "stop_task_receipt", b)
	for _, state := range domain.AllTaskCancellationStates {
		t.Run(string(state), func(t *testing.T) {
			value := c
			value.State = state
			if state != domain.TaskCancellationRequested {
				value.Acknowledgement = &domain.TaskCancellationAcknowledgement{ID: "ack-1", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: state, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt.Add(time.Minute)}
			}
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			b, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, "task_cancellation_"+string(state), b)
		})
	}
	for _, mutate := range []func(*domain.TaskCancellation){
		func(v *domain.TaskCancellation) { v.State = "" },
		func(v *domain.TaskCancellation) { v.State = domain.TaskCancellationConfirmed },
		func(v *domain.TaskCancellation) { v.Target.Runs = nil },
		func(v *domain.TaskCancellation) { v.TargetDigest = "sha256:forged" },
		func(v *domain.TaskCancellation) { v.FenceRevision = 0 },
	} {
		bad := c
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid cancellation: %+v", bad)
		}
	}
}
