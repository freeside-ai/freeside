package ward

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestTaskCancellationVerificationJoinsAndFencesEveryCommand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newFakeRuntime(t)
		owned, err := NewVerificationOwnership(filepath.Join(t.TempDir(), "owned"), runtime)
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		command := func(ctx context.Context, _ string, _ []string, _ int64) (verify.StepResult, error) {
			close(entered)
			<-ctx.Done()
			return verify.StepResult{}, ctx.Err()
		}
		room := newProjectImageRoom("container", verificationProjectImage(t, []string{"prepare"}), runtime, command, command, 1024)
		if err := owned.Bind(room, "task", "run", "verify"); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := room.Run(t.Context(), t.TempDir(), []string{"verify"}); done <- err }()
		<-entered
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := owned.StopRun(ctx, "task", "run"); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("command did not cancel: %v", err)
		}
		if _, err := room.ReadRecipe(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("post-stop launch: %v", err)
		}
	})
}

func TestTaskCancellationVerificationRestartRetainsUncertainty(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted-before-cid", true: "joined-before-cleanup"}[joined], func(t *testing.T) {
			runtime := newFakeRuntime(t)
			root := filepath.Join(t.TempDir(), "owned")
			owned, err := NewVerificationOwnership(root, runtime)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := newOwnershipLabel()
			if err != nil {
				t.Fatal(err)
			}
			_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(joined); err != nil {
				t.Fatal(err)
			}
			runtime.ctrs["owned"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{owner}}, stopped: true, created: "owned-created"}
			runtime.ctrs["foreign"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{{Key: "other", Value: "owner"}}}, stopped: true, created: "foreign-created"}
			restarted, err := NewVerificationOwnership(root, runtime)
			if err != nil {
				t.Fatal(err)
			}
			err = restarted.StopRun(t.Context(), "task", "run")
			if (err == nil) != joined {
				t.Fatalf("joined=%v, stop=%v", joined, err)
			}
			if runtime.ctrs["owned"] != nil || runtime.ctrs["foreign"] == nil {
				t.Fatal("cleanup did not preserve exact ownership")
			}
		})
	}
}

func TestTaskCancellationVerificationRejectsLyingDeleteAndDamagedBinding(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(map[bool]string{false: "listed", true: "omitted"}[omit], func(t *testing.T) {
			runtime := newFakeRuntime(t)
			owned, err := NewVerificationOwnership(filepath.Join(t.TempDir(), "owned"), runtime)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := newOwnershipLabel()
			if err != nil {
				t.Fatal(err)
			}
			_, cid, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(true); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cid, []byte("owned"), 0o600); err != nil {
				t.Fatal(err)
			}
			runtime.ctrs["owned"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{owner}}, stopped: true, created: "owned-created"}
			runtime.onDeleteContainer = func(string) (bool, error) { return true, nil }
			if omit {
				runtime.onListContainers = func([]ContainerSummary) ([]ContainerSummary, error) { return nil, nil }
			}
			if err := owned.StopRun(t.Context(), "task", "run"); err == nil {
				t.Fatal("lying delete confirmed quiescence")
			}
			if err := owned.StopRun(t.Context(), "wrong-task", "run"); err == nil {
				t.Fatal("retargeted record authorized cleanup")
			}
		})
	}
}

func TestTaskCancellationCoverageSurvivesRestartAndEpochChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.json")
	coverage, err := OpenCancellationCoverage(path, "epoch", []domain.TaskID{"old"})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Covers("old", "epoch") == nil || coverage.Covers("new", "restored") == nil {
		t.Fatal("unknown ownership was accepted")
	}
	restored, err := OpenCancellationCoverage(path, "epoch", []domain.TaskID{"old", "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Covers("new", "epoch"); err != nil {
		t.Fatal(err)
	}
}
