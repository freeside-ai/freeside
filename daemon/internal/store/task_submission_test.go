package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const taskSubmissionDigest = domain.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")

// seedTaskSubmissionTarget creates the task and specification run a submission
// record's foreign keys reference, so a PutTaskSubmission has real rows to bind.
func seedTaskSubmissionTarget(t *testing.T, ctx context.Context, s *store.Store) (domain.TaskID, domain.RunID) {
	t.Helper()
	var taskID domain.TaskID
	runID := domain.RunID("run-submission-1")
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "project", taskIssueSource())
		if err != nil {
			return err
		}
		taskID = task.ID
		return tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project", TaskID: taskID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		})
	}); err != nil {
		t.Fatal(err)
	}
	return taskID, runID
}

func TestPutTaskSubmissionWriteOnceAndConvergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, runID := seedTaskSubmissionTarget(t, ctx, s)
	submission := domain.TaskSubmission{
		CommandID: "cmd-sub-1", DeviceID: "device-1", ProjectID: "project",
		SourceDigest: taskSubmissionDigest, TaskID: taskID, SpecificationRunID: runID,
		Name: domain.DisplayName{Text: "First name", Source: domain.DisplayNameSourceOperator},
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutTaskSubmission(ctx, submission) }); err != nil {
		t.Fatal(err)
	}
	var (
		got  domain.TaskSubmission
		snap store.Snapshot
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, snap, err = tx.GetTaskSubmissionSnapshot(ctx, "cmd-sub-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != submission {
		t.Fatalf("submission = %+v, want %+v", got, submission)
	}
	if snap.EntityVersion != 1 || snap.AsOfRevision < 1 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// A byte-identical replay converges without error and no second write.
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutTaskSubmission(ctx, submission) }); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	// A changed body under the same command id is an immutable conflict.
	changed := submission
	changed.Name = domain.DisplayName{Text: "Second name", Source: domain.DisplayNameSourceOperator}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutTaskSubmission(ctx, changed) }); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changed body error = %v, want ErrImmutableConflict", err)
	}
}

// TestPutCommandRejectsTaskSubmissionCommandID covers the cross-kind rule from
// the decision side: a command id already recorded as a task submission cannot
// be reused for a decision command. PutCommand's cross-kind probe rejects it
// before the attention-item lookup, so no item scaffolding is needed. The
// reverse direction (a submission reusing a decision id) is covered end to end
// by the signet acceptance tests.
func TestPutCommandRejectsTaskSubmissionCommandID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, runID := seedTaskSubmissionTarget(t, ctx, s)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutTaskSubmission(ctx, domain.TaskSubmission{
			CommandID: "cmd-shared", DeviceID: "device-1", ProjectID: "project",
			SourceDigest: taskSubmissionDigest, TaskID: taskID, SpecificationRunID: runID,
			Name: domain.DisplayName{Text: "Name", Source: domain.DisplayNameSourceOperator},
		})
	}); err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-shared", DeviceID: "device-1", ItemID: "item-1", ItemVersion: 1, Action: domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, command) }); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("cross-kind decision command error = %v, want ErrImmutableConflict", err)
	}
}

// TestPutTaskSubmissionRejectsDecisionCommandID covers the cross-kind rule from
// the submission side: a command id already recorded as a decision command
// cannot be reused for a task submission. It reuses the shared fixtures to seed
// a real decision command under that id.
func TestPutTaskSubmissionRejectsDecisionCommandID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedComprehensionDeps(t, ctx, s, f)
	taskID, runID := seedTaskSubmissionTarget(t, ctx, s)
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutTaskSubmission(ctx, domain.TaskSubmission{
			CommandID: f.command.CommandID, DeviceID: "device-1", ProjectID: "project",
			SourceDigest: taskSubmissionDigest, TaskID: taskID, SpecificationRunID: runID,
			Name: domain.DisplayName{Text: "Name", Source: domain.DisplayNameSourceOperator},
		})
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("reusing a decision command_id for a submission = %v, want ErrImmutableConflict", err)
	}
}

func TestGetTaskByIntakeKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTaskByIntakeKey(ctx, "project", "issue:123:42")
		if err != nil {
			return err
		}
		if task.ID != taskID {
			t.Fatalf("task = %s, want %s", task.ID, taskID)
		}
		if _, err := tx.GetTaskByIntakeKey(ctx, "other", "issue:123:42"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("other project error = %v, want ErrNotFound", err)
		}
		if _, err := tx.GetTaskByIntakeKey(ctx, "project", "issue:999:1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown key error = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
