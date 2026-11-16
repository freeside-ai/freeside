package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// switchableGate is a readiness gate a test turns on and off. While on it
// refuses every project with trust_profile failed.
type switchableGate struct {
	mu       sync.Mutex
	notReady bool
}

func (g *switchableGate) set(notReady bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.notReady = notReady
}

func (g *switchableGate) gate(_ context.Context, _ *store.ReadTx, project domain.ProjectID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.notReady {
		return nil
	}
	return &domain.ProjectNotReadyError{ProjectID: project, NotPassed: []domain.ProjectReadinessResult{{
		Check: domain.ProjectReadinessTrustProfile, Status: domain.ProjectReadinessFailed, Detail: "no approved trust profile",
	}}}
}

// TestSubmitTaskRefusedWhileProjectNotReady: a paired client's submission
// for a project that is not ready is refused, names the failed check, and
// writes no task or submission row; a replay of one the daemon already
// recorded is not refused.
func TestSubmitTaskRefusedWhileProjectNotReady(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	gate := &switchableGate{}
	service, s, _ := newGatedSubmitTaskHarness(t, gate.gate)
	recorded, err := service.Submit(ctx, submitCmd("cmd-ready", "project-1", "Ready work", ""))
	if err != nil {
		t.Fatal(err)
	}

	gate.set(true)
	_, err = service.Submit(ctx, submitCmd("cmd-not-ready", "project-1", "New work", ""))
	if !errors.Is(err, domain.ErrProjectNotReady) || !strings.Contains(err.Error(), "trust_profile failed") {
		t.Fatalf("submit = %v, want a refusal naming trust_profile", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetTaskSubmission(ctx, "cmd-not-ready"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refused submission recorded: %v", err)
		}
		if _, err := tx.GetTaskByIntakeKey(ctx, "project-1", "submission:client:cmd-not-ready"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refused submission created a task: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	replayed, err := service.Submit(ctx, submitCmd("cmd-ready", "project-1", "Ready work", ""))
	if err != nil || replayed.Submission.TaskID != recorded.Submission.TaskID {
		t.Fatalf("replay = %+v, %v; want the recorded task %s", replayed.Submission, err, recorded.Submission.TaskID)
	}
}
