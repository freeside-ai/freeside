package ward

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

type stoppedJudgmentDriver struct{ complete func(context.Context) error }

func (d stoppedJudgmentDriver) Complete(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
	return inference.Response{}, d.complete(ctx)
}

func TestTaskCancellationJudgmentReturnIsNotNativeProof(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, done := make(chan struct{}), make(chan struct{})
		root := filepath.Join(t.TempDir(), "judgments")
		driver := stoppedJudgmentDriver{complete: func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }}
		owned, err := NewTaskJudgments(root, driver)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(WithTaskOwner(t.Context(), "task", "run"))
		go func() { defer close(done); _, _ = owned.Complete(ctx, inference.Request{SiteID: "task-name"}, "") }()
		<-entered
		cancel()
		if err := owned.ConfirmRun(t.Context(), "task", "run"); err == nil {
			t.Fatal("in-flight call confirmed")
		}
		<-done
		restarted, err := NewTaskJudgments(root, driver)
		if err != nil {
			t.Fatal(err)
		}
		if err := restarted.ConfirmRun(t.Context(), "task", "run"); err == nil {
			t.Fatal("plain Complete return invented native-process proof")
		}
	})
}

func TestTaskCancellationJudgmentQueuedBeforeProviderEntry(t *testing.T) {
	owned, err := NewTaskJudgments(filepath.Join(t.TempDir(), "judgments"), stoppedJudgmentDriver{complete: func(context.Context) error { t.Fatal("cancelled task entered provider"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(WithTaskOwner(t.Context(), "task", "run"))
	cancel()
	if _, err := owned.Complete(ctx, inference.Request{SiteID: "task-name"}, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := owned.ConfirmRun(t.Context(), "task", "run"); err != nil {
		t.Fatal(err)
	}
}
