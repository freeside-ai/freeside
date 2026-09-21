package signet_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func seedStopTask(t *testing.T, s *store.Store, phases ...string) domain.TaskID {
	t.Helper()
	var id domain.TaskID
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(t.Context(), "project-1", domain.SpecificationSource{Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 1}})
		if err != nil {
			return err
		}
		id = task.ID
		for _, phase := range phases {
			if err := tx.PutRun(t.Context(), domain.Run{ID: domain.RunID("run-" + phase), TaskID: id, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{{ID: domain.StageID("stage-" + phase), RunID: domain.RunID("run-" + phase), Name: phase, Attempts: []domain.Attempt{}}}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func stopCommand(t *testing.T, s *store.Store, id domain.TaskID, commandID string) signet.ClientCommand {
	t.Helper()
	state, err := s.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return signet.ClientCommand{CommandID: commandID, DeviceID: "device-1", Kind: domain.CommandKindStopTask, ExpectedEntityVersion: state.Revision, StopTask: signet.StopTaskPayload{TaskID: id, ProjectID: "project-1", ExpectedSyncEpoch: state.SyncEpoch}}
}

// advanceRevision commits an unrelated write, raising the global revision the
// way an engine observation refresh does while an agent runs. It leaves the
// sync epoch untouched.
func advanceRevision(t *testing.T, s *store.Store, marker string) {
	t.Helper()
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutDevice(t.Context(), domain.Device{ID: domain.DeviceID("filler-" + marker), DisplayName: "filler", Status: domain.DeviceActive, PairedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStopTaskAcceptsRevisionMovedByUnrelatedWrite(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s, "implementation")
	// The client prepares against the current revision, then unrelated writes
	// advance it before the Stop lands. This reproduces the run-70 live-task
	// case; it fails with a StaleTaskError before the acceptance rule loosens.
	command := stopCommand(t, s, id, "stop-after-writes")
	advanceRevision(t, s, "a")
	advanceRevision(t, s, "b")
	state, err := s.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision <= command.ExpectedEntityVersion {
		t.Fatalf("unrelated writes did not advance the revision: %d not past %d", state.Revision, command.ExpectedEntityVersion)
	}
	result, err := svc.Submit(t.Context(), command)
	if err != nil {
		t.Fatalf("stop below current revision rejected: %v", err)
	}
	if result.Stop.Cancellation.State != domain.TaskCancellationRequested {
		t.Fatalf("cancellation: %+v", result.Stop.Cancellation)
	}
	// A task_stop_commands row exists and keeps the client's prepared version.
	var row domain.StopTaskReceipt
	if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		r, _, err := tx.GetStopTaskReceipt(t.Context(), command.CommandID)
		row = r
		return err
	}); err != nil {
		t.Fatalf("no task_stop_commands row: %v", err)
	}
	if row.ExpectedEntityVersion != command.ExpectedEntityVersion {
		t.Fatalf("receipt rewrote the prepared version: %d not %d", row.ExpectedEntityVersion, command.ExpectedEntityVersion)
	}
}

func TestStopTaskAcceptanceAndImmutableReplay(t *testing.T) {
	for _, phases := range [][]string{nil, {"specification"}, {"implementation", "review", "verification", "older-child"}} {
		t.Run("runs="+strconv.Itoa(len(phases)), func(t *testing.T) {
			svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
			id := seedStopTask(t, s, phases...)
			command := stopCommand(t, s, id, "stop-1")
			result, err := svc.Submit(t.Context(), command)
			if err != nil {
				t.Fatal(err)
			}
			c := result.Stop.Cancellation
			if c.State != domain.TaskCancellationRequested || c.FenceRevision != command.ExpectedEntityVersion+1 || len(c.Target.Runs) != len(phases) || c.Target.EpisodeOrdinal != 0 {
				t.Fatalf("cancellation: %+v", c)
			}
			var before domain.Task
			if err := s.Read(t.Context(), func(tx *store.ReadTx) error { var err error; before, err = tx.GetTask(t.Context(), id); return err }); err != nil {
				t.Fatal(err)
			}
			if before.Cancellation == nil || domain.TaskWIP(before) {
				t.Fatalf("task: %+v", before)
			}
			ack := domain.TaskCancellationAcknowledgement{ID: "failed", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationFailed, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt.Add(time.Second)}
			for _, state := range []domain.TaskCancellationState{domain.TaskCancellationFailed, domain.TaskCancellationConfirmed} {
				ack.ID = string(state)
				ack.State = state
				ack.RecordedAt = ack.RecordedAt.Add(time.Second)
				if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
					changed, err := tx.AcknowledgeTaskCancellation(t.Context(), ack)
					if !changed && err == nil {
						t.Fatal("first ack did not change state")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				replayed, err := svc.Submit(t.Context(), command)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(result, replayed) {
					t.Fatalf("receipt changed: %+v", replayed)
				}
				distinct, err := svc.Submit(t.Context(), stopCommand(t, s, id, "another-"+string(state)))
				if err != nil {
					t.Fatal(err)
				}
				if distinct.Stop.Cancellation.RequestID != c.RequestID || distinct.Stop.Cancellation.State != state {
					t.Fatalf("duplicate reset state: %+v", distinct.Stop)
				}
			}
			noChange := errors.New("no change")
			beforeRevision, _ := s.ServerState(t.Context())
			err = s.Write(t.Context(), func(tx *store.WriteTx) error {
				changed, err := tx.AcknowledgeTaskCancellation(t.Context(), ack)
				if err != nil {
					return err
				}
				if changed {
					t.Fatal("ack replay changed")
				}
				return noChange
			})
			if !errors.Is(err, noChange) {
				t.Fatal(err)
			}
			afterRevision, _ := s.ServerState(t.Context())
			if beforeRevision != afterRevision {
				t.Fatal("ack replay advanced revision")
			}
			ack.ID = "regress"
			ack.State = domain.TaskCancellationFailed
			if err := s.Write(t.Context(), func(tx *store.WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(t.Context(), ack); return err }); !errors.Is(err, domain.ErrImmutableTransition) {
				t.Fatalf("confirmed regression: %v", err)
			}
		})
	}
}

func TestStopTaskRejectsChangedRequestsAndStaleBindings(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s)
	command := stopCommand(t, s, id, "stop-1")
	if _, err := svc.Submit(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*signet.ClientCommand){
		func(c *signet.ClientCommand) { c.StopTask.TaskID = "foreign" },
		func(c *signet.ClientCommand) { c.StopTask.ProjectID = "foreign" },
		func(c *signet.ClientCommand) { c.StopTask.ExpectedSyncEpoch = "foreign" },
		func(c *signet.ClientCommand) { c.ExpectedEntityVersion++ },
	} {
		changed := command
		mutate(&changed)
		if _, err := svc.Submit(t.Context(), changed); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("changed input: %v", err)
		}
	}
	// A version above the current revision can't have been observed, so it is
	// rejected with the replacement snapshot. An older or equal version is now
	// accepted; see TestStopTaskAcceptsRevisionMovedByUnrelatedWrite.
	state, err := s.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tooNew := command
	tooNew.CommandID = "too-new"
	tooNew.ExpectedEntityVersion = state.Revision + 1
	var conflict *signet.StaleTaskError
	if _, err := svc.Submit(t.Context(), tooNew); !errors.As(err, &conflict) {
		t.Fatalf("too-new version: %v", err)
	}
	if conflict.ReplacementTask.EntityVersion != state.Revision || conflict.ReplacementTask.Task.Cancellation == nil {
		t.Fatalf("replacement: %+v", conflict)
	}
	fresh := stopCommand(t, s, id, "epoch")
	if _, err := s.NewEpoch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(t.Context(), fresh); !errors.As(err, &conflict) {
		t.Fatalf("epoch: %v", err)
	}
	if _, err := svc.Submit(t.Context(), command); err != nil {
		t.Fatalf("old receipt after epoch: %v", err)
	}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		device, err := tx.GetDevice(t.Context(), command.DeviceID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		device.Status = domain.DeviceRevoked
		device.RevokedAt = &now
		return tx.PutDevice(t.Context(), device)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(t.Context(), command); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("revoked replay: %v", err)
	}
}

func TestStopTaskConcurrentDevicesAndLateAcknowledgement(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s, "old-child")
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutDevice(t.Context(), domain.Device{ID: "device-2", DisplayName: "second", Status: domain.DeviceActive, PairedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	command := stopCommand(t, s, id, "first")
	var wg sync.WaitGroup
	results := make(chan signet.CommandResult, 8)
	for range 8 {
		wg.Go(func() {
			result, err := svc.Submit(context.Background(), command)
			if err != nil {
				t.Error(err)
			} else {
				results <- result
			}
		})
	}
	wg.Wait()
	close(results)
	var first signet.CommandResult
	for result := range results {
		if first.Stop == nil {
			first = result
		}
		if !reflect.DeepEqual(result, first) {
			t.Fatal("concurrent replay differs")
		}
	}
	other := stopCommand(t, s, id, "second")
	other.DeviceID = "device-2"
	second, err := svc.Submit(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if second.Stop.Cancellation.RequestID != first.Stop.Cancellation.RequestID {
		t.Fatal("devices created duplicate work")
	}
	other.CommandID = command.CommandID
	if _, err := svc.Submit(t.Context(), other); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("foreign replay: %v", err)
	}
	// This unit supplies the fence; runtime enforcement is a downstream unit.
	// Simulate a changed target and prove old runtime evidence cannot confirm it.
	seedStopTask(t, s, "new-child")
	c := first.Stop.Cancellation
	ack := domain.TaskCancellationAcknowledgement{ID: "late", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationConfirmed, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt.Add(time.Second)}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(t.Context(), ack); return err }); !errors.Is(err, store.ErrCancellationBinding) {
		t.Fatalf("late ack: %v", err)
	}
	third, err := svc.Submit(t.Context(), stopCommand(t, s, id, "third"))
	if err != nil {
		t.Fatal(err)
	}
	if third.Stop.Cancellation.RequestID == c.RequestID || len(third.Stop.Cancellation.Target.Runs) != 2 {
		t.Fatal("new target reused old fence")
	}
}

func TestStopTaskCommandWireRecord(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s)
	result, err := svc.Submit(t.Context(), stopCommand(t, s, id, "wire"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Record struct {
			Kind string `json:"kind"`
			domain.StopTaskReceipt
		} `json:"record"`
		Revision int64 `json:"revision"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Record.Kind != "stop_task" || !reflect.DeepEqual(decoded.Record.StopTaskReceipt, *result.Stop) || decoded.Revision != result.Revision {
		t.Fatalf("wire: %s", body)
	}
}

func TestStopTaskConcurrentDeviceRequestsConverge(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s)
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutDevice(t.Context(), domain.Device{ID: "device-2", DisplayName: "second", Status: domain.DeviceActive, PairedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	// Both devices prepare against the same revision and stop at once. The first
	// accepted write advances the revision, but the second's now-older version is
	// no longer rejected: both are accepted and share one fence.
	first := stopCommand(t, s, id, "device-one-stop")
	second := first
	second.CommandID, second.DeviceID = "device-two-stop", "device-2"
	type outcome struct {
		result signet.CommandResult
		err    error
	}
	results := make(chan outcome, 2)
	for _, command := range []signet.ClientCommand{first, second} {
		go func() {
			result, err := svc.Submit(t.Context(), command)
			results <- outcome{result, err}
		}()
	}
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("both devices should be accepted: %v, %v", a.err, b.err)
	}
	// Distinct commands, one shared cancellation, no duplicate work.
	if a.result.Stop.Cancellation.RequestID != b.result.Stop.Cancellation.RequestID {
		t.Fatalf("devices created duplicate cancellations: %s vs %s", a.result.Stop.Cancellation.RequestID, b.result.Stop.Cancellation.RequestID)
	}
	var task domain.Task
	if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetTask(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if task.Cancellation == nil || task.Cancellation.RequestID != a.result.Stop.Cancellation.RequestID {
		t.Fatalf("task did not converge on one cancellation: %+v", task.Cancellation)
	}
}

func TestStopTaskRestartPreservesReceiptAndFence(t *testing.T) {
	f := newRunFixture(t)
	id := seedStopTask(t, f.store, "specification")
	command := stopCommand(t, f.store, id, "survives-restart")
	accepted, err := f.service.Submit(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, f.dbPath, store.Options{})
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := signet.NewService(reopened)
	before, _ := reopened.ServerState(t.Context())
	replay, err := restarted.Submit(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := reopened.ServerState(t.Context())
	if !reflect.DeepEqual(accepted, replay) || before != after {
		t.Fatal("restart changed receipt or revision")
	}
	if err := reopened.Read(t.Context(), func(tx *store.ReadTx) error {
		task, err := tx.GetTask(t.Context(), id)
		if err == nil && (task.Cancellation == nil || task.Cancellation.State != domain.TaskCancellationRequested) {
			t.Fatal("restart inferred quiescence")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStopTaskPreservesLifecycleAndRejectsPriorEpisodeAcknowledgement(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s, "specification", "implementation")
	start := time.Now().UTC()
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error { return tx.RecordTaskStart(t.Context(), "run-specification", start) }); err != nil {
		t.Fatal(err)
	}
	original, err := svc.Submit(t.Context(), stopCommand(t, s, id, "active"))
	if err != nil {
		t.Fatal(err)
	}
	if original.Stop.Cancellation.Target.EpisodeOrdinal != 1 {
		t.Fatal("missing episode binding")
	}
	read := func() domain.Task {
		var task domain.Task
		if err := s.Read(t.Context(), func(tx *store.ReadTx) error { var err error; task, err = tx.GetTask(t.Context(), id); return err }); err != nil {
			t.Fatal(err)
		}
		return task
	}
	if !domain.TaskWIP(read()) {
		t.Fatal("acceptance released WIP")
	}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		_, err := tx.AbandonTask(t.Context(), id, start.Add(time.Second))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	terminal := read()
	result, err := svc.Submit(t.Context(), stopCommand(t, s, id, "terminal-looking"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Stop.Cancellation.State != domain.TaskCancellationRequested || !reflect.DeepEqual(read().LifecycleFacts, terminal.LifecycleFacts) {
		t.Fatal("terminal-looking work fabricated confirmation or changed history")
	}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordTaskStart(t.Context(), "run-implementation", start.Add(2*time.Second))
	}); !errors.Is(err, store.ErrTaskCancellationFenced) {
		t.Fatalf("late start crossed cancellation fence: %v", err)
	}
	// Pre-upgrade data could contain an unrestricted post-abandon start.
	// Seed that historical fact explicitly; the live admission door refuses it.
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordTaskLifecycleFact(t.Context(), id, domain.TaskLifecycleFact{
			Kind: domain.TaskLifecycleStarted, RunID: "run-implementation",
			SourceID: "start:run-implementation", RecordedAt: start.Add(2 * time.Second),
		})
	}); err != nil {
		t.Fatal(err)
	}
	c := original.Stop.Cancellation
	ack := domain.TaskCancellationAcknowledgement{ID: "old-episode", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationConfirmed, EvidenceDigest: c.TargetDigest, RecordedAt: start.Add(3 * time.Second)}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(t.Context(), ack); return err }); !errors.Is(err, store.ErrCancellationBinding) {
		t.Fatalf("old episode ack: %v", err)
	}
}

func TestStopTaskEpochChangeRequiresNewFence(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s)
	command := stopCommand(t, s, id, "old-epoch")
	old, err := svc.Submit(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	c := old.Stop.Cancellation
	ack := domain.TaskCancellationAcknowledgement{ID: "old-confirmation", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationConfirmed, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt.Add(time.Second)}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(t.Context(), ack); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewEpoch(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := svc.Submit(t.Context(), command)
	if err != nil || !reflect.DeepEqual(replay, old) {
		t.Fatalf("historical replay: %v", err)
	}
	ack.ID = "late-old-epoch"
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(t.Context(), ack); return err }); !errors.Is(err, store.ErrCancellationBinding) {
		t.Fatalf("late old-epoch ack: %v", err)
	}
	fresh, err := svc.Submit(t.Context(), stopCommand(t, s, id, "new-epoch"))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Stop.Cancellation.RequestID == c.RequestID || fresh.Stop.Cancellation.State != domain.TaskCancellationRequested {
		t.Fatal("reused pre-restore quiescence")
	}
}
