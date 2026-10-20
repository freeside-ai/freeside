package engine

import (
	"context"
	"errors"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ResumeTaskInvocation applies the task fence before concrete stage recovery
// can restart a pre-journal invocation. It uses the same launch ordering as a
// new dispatch, including a task cancellation context for the new pipeline.
func (e *Engine) ResumeTaskInvocation(ctx context.Context, id domain.RunID, launch func(context.Context) error) error {
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, id)
		return err
	}); err != nil {
		return err
	}
	err := e.launchTaskInvocation(ctx, run, launch)
	if errors.Is(err, store.ErrTaskCancellationFenced) {
		return stage.ErrTaskCancelled
	}
	if errors.Is(err, store.ErrTaskWorkflowClosed) {
		return errors.Join(stage.ErrWorkflowClosed, err)
	}
	return err
}

// RecoverTaskInvocation observes an existing handoff under Stop's registration.
// Administrative closure grants no launch permission but does not prevent its
// authenticated result and real resource release from being recovered.
func (e *Engine) RecoverTaskInvocation(ctx context.Context, id domain.RunID, observe func(context.Context) error) error {
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, id)
		return err
	}); err != nil {
		return err
	}
	workCtx, finish, err := e.registerTaskWork(ctx, run, true)
	if errors.Is(err, store.ErrTaskCancellationFenced) {
		return stage.ErrTaskCancelled
	}
	if err != nil {
		return err
	}
	defer finish()
	err = observe(stage.WithTaskCancellation(workCtx, e.taskExecution(run.TaskID).ctx))
	if err != nil && workCtx.Err() != nil && ctx.Err() == nil {
		if fence := e.store.Read(ctx, func(tx *store.ReadTx) error {
			return requireTaskNotCancelled(ctx, tx, id)
		}); errors.Is(fence, store.ErrTaskCancellationFenced) {
			return stage.ErrTaskCancelled
		}
	}
	return err
}

type taskExecution struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	active int
	idle   chan struct{}
}

// BeginRunWork registers owned work for Stop and cleanup. Registration grants
// no dispatch authority: engine preparation must finish before claimRunDispatch.
func (e *Engine) BeginRunWork(ctx context.Context, id domain.RunID) (context.Context, func(), error) {
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, id)
		return err
	}); err != nil {
		return nil, nil, err
	}
	return e.beginTaskWork(ctx, run)
}

func (e *Engine) taskExecution(id domain.TaskID) *taskExecution {
	e.taskExecutionMu.Lock()
	defer e.taskExecutionMu.Unlock()
	if e.taskExecutions == nil {
		e.taskExecutions = make(map[domain.TaskID]*taskExecution)
	}
	if running := e.taskExecutions[id]; running != nil {
		return running
	}
	ctx, cancel := context.WithCancel(context.Background())
	running := &taskExecution{ctx: ctx, cancel: cancel, idle: make(chan struct{})}
	close(running.idle)
	e.taskExecutions[id] = running
	return running
}

// CommitTaskStop orders the accepting transaction against invocation launch.
// Registration and dispatch claims hold this lock, never provider execution
// or teardown. The database remains the durable authority after a restart.
func (e *Engine) CommitTaskStop(ctx context.Context, id domain.TaskID, commit func() error) error {
	running := e.taskExecution(id)
	running.mu.Lock()
	defer running.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := commit(); err != nil {
		return err
	}
	return e.store.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTask(ctx, id)
		if err == nil && task.Cancellation != nil {
			running.cancel()
		}
		return err
	})
}

// CommitTaskWorkflowClosure orders live closure against engine dispatch claims.
// A winning claim remains valid through downstream preparation and execution;
// closure neither revokes it nor cancels the registered work.
func (e *Engine) CommitTaskWorkflowClosure(ctx context.Context, id domain.TaskID, commit func() error) error {
	running := e.taskExecution(id)
	running.mu.Lock()
	defer running.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return commit()
}

// claimRunDispatch is the last engine step before each provider entry or stage
// launch. The mutex orders the claim, never provider execution, against closure.
// Every engine retry must claim again; the database fences claims after restart.
func (e *Engine) claimRunDispatch(ctx context.Context, id domain.RunID) error {
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, id)
		return err
	}); err != nil {
		return err
	}
	running := e.taskExecution(run.TaskID)
	running.mu.Lock()
	defer running.mu.Unlock()
	return e.store.Read(ctx, func(tx *store.ReadTx) error {
		return requireTaskExecutionOpen(ctx, tx, id)
	})
}

func (e *Engine) launchTaskInvocation(ctx context.Context, run domain.Run, launch func(context.Context) error) error {
	workCtx, finish, err := e.beginTaskWork(ctx, run)
	if err != nil {
		return err
	}
	defer finish()
	if err := e.claimRunDispatch(workCtx, run.ID); err != nil {
		return err
	}
	err = launch(stage.WithTaskCancellation(workCtx, e.taskExecution(run.TaskID).ctx))
	if err != nil && workCtx.Err() != nil && ctx.Err() == nil {
		// A task Stop is an ordinary hold, not failure of the shared loop.
		if fence := e.store.Read(ctx, func(tx *store.ReadTx) error {
			return requireTaskExecutionOpen(ctx, tx, run.ID)
		}); errors.Is(fence, store.ErrTaskCancellationFenced) {
			return fence
		}
	}
	return err
}

func (e *Engine) beginTaskWork(ctx context.Context, run domain.Run) (context.Context, func(), error) {
	return e.registerTaskWork(ctx, run, false)
}

func (e *Engine) registerTaskWork(ctx context.Context, run domain.Run, observationOnly bool) (context.Context, func(), error) {
	running := e.taskExecution(run.TaskID)
	running.mu.Lock()
	defer running.mu.Unlock()
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		if observationOnly {
			return requireTaskNotCancelled(ctx, tx, run.ID)
		}
		return requireTaskExecutionOpen(ctx, tx, run.ID)
	}); err != nil {
		return nil, nil, err
	}
	workCtx, cancel := context.WithCancel(ward.WithTaskOwner(ctx, run.TaskID, run.ID))
	stop := context.AfterFunc(running.ctx, cancel)
	if running.active == 0 {
		running.idle = make(chan struct{})
	}
	running.active++
	return workCtx, func() {
		stop()
		cancel()
		running.mu.Lock()
		defer running.mu.Unlock()
		running.active--
		if running.active == 0 {
			close(running.idle)
		}
	}, nil
}

func (e *Engine) stopTaskWork(ctx context.Context, id domain.TaskID) error {
	running := e.taskExecution(id)
	running.mu.Lock()
	running.cancel()
	idle := running.idle
	running.mu.Unlock()
	select {
	case <-idle:
		return nil
	default:
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
