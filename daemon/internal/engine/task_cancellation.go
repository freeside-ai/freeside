package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TaskCancellationRuntime joins the concrete runtime's ownership inventory to
// a captured task fence. StopRun must stop and prove the absence of every
// stage, review, verification, and advisory child of the supplied run. Neither
// a terminal display state nor absence from a process-local map is proof.
type TaskCancellationRuntime struct {
	StopRun func(context.Context, domain.Run, []domain.ReviewRequestRecord) error
	Timeout time.Duration
}

func WithTaskCancellationRuntime(runtime TaskCancellationRuntime) Option {
	return func(e *Engine) error {
		if runtime.StopRun == nil || runtime.Timeout <= 0 {
			return errors.New("task cancellation needs a runtime proof adapter and positive timeout")
		}
		e.cancellation.runtime = runtime
		return nil
	}
}

type taskCancellationCoordinator struct {
	mu       sync.Mutex
	runtime  TaskCancellationRuntime
	attempts map[string]*taskStopAttempt
}

type taskStopAttempt struct {
	done     chan struct{}
	deadline time.Time
	err      error
}

type cancellationRun struct {
	run     domain.Run
	reviews []domain.ReviewRequestRecord
}

// StopTaskReviews stops both concrete review arms belonging to a recorded
// primary round. Shadow requests have their own deterministic invocation ID
// and private request journal entry, not a routed ReviewRequestRecord. Both
// arms must settle even when the other has completed or fails to stop.
func StopTaskReviews(ctx context.Context, st *store.Store, review domain.ReviewRequestRecord, primary, shadow exec.ReviewSource) error {
	var children sync.WaitGroup
	var outcomes sync.Mutex
	var result error
	for _, child := range []struct {
		id     domain.InvocationID
		source exec.ReviewSource
	}{
		{review.InvocationID, primary},
		{ProductionShadowReviewInvocationID(review.RunID, review.Round), shadow},
	} {
		children.Go(func() {
			err := stopTaskReview(ctx, st, child.id, child.source)
			outcomes.Lock()
			result = errors.Join(result, err)
			outcomes.Unlock()
		})
	}
	children.Wait()
	return result
}

func stopTaskReview(ctx context.Context, st *store.Store, id domain.InvocationID, source exec.ReviewSource) error {
	if source == nil {
		// An optional source may have been removed after a daemon restart.
		// Concrete sources journal before launch, so only exact row absence
		// proves this arm never entered; a retained row needs its adapter.
		return st.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetCodexReviewRequest(ctx, string(id))
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return errors.Join(err, fmt.Errorf("review %s has no owned cancellation adapter", id))
		})
	}
	adapter, ok := source.(interface {
		CancelReview(context.Context, domain.InvocationID) error
	})
	if !ok {
		return errors.New("review source has no owned cancellation adapter")
	}
	err := adapter.CancelReview(ctx, id)
	if errors.Is(err, exec.ErrUnknownInvocation) {
		return nil // The concrete source's exact pre-launch journal is absent.
	}
	return err
}

func requireTaskExecutionOpen(ctx context.Context, tx *store.ReadTx, runID domain.RunID) error {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if task.Cancellation != nil {
		return store.ErrTaskCancellationFenced
	}
	return nil
}

// ReconcileTaskCancellations discovers every committed Stop without waiting
// behind provider execution or another task's teardown. A worker's timeout
// records failed_to_stop; it cannot authorize confirmation. A worker that
// ignores cancellation remains registered until it actually returns, so later
// passes cannot accumulate duplicate stop attempts.
func (e *Engine) ReconcileTaskCancellations(ctx context.Context) error {
	e.cancellation.mu.Lock()
	defer e.cancellation.mu.Unlock()
	var tasks []store.Snapshotted[domain.Task]
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		tasks, err = tx.ListTasks(ctx)
		return err
	}); err != nil {
		return err
	}
	if e.cancellation.attempts == nil {
		e.cancellation.attempts = make(map[string]*taskStopAttempt)
	}
	for _, snapshot := range tasks {
		c := snapshot.Value.Cancellation
		if c == nil || c.State == domain.TaskCancellationConfirmed {
			continue
		}
		e.taskExecution(c.Target.TaskID).cancel()
		attempt := e.cancellation.attempts[c.RequestID]
		if attempt == nil {
			runs, err := e.cancellationRuns(ctx, *c)
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				if err := e.acknowledgeTaskStop(ctx, *c, true); err != nil {
					return err
				}
				continue
			}
			runtime := e.cancellation.runtime
			if runtime.StopRun == nil {
				if err := e.acknowledgeTaskStop(ctx, *c, false); err != nil {
					return err
				}
				continue
			}
			attempt = &taskStopAttempt{done: make(chan struct{}), deadline: time.Now().Add(runtime.Timeout)}
			e.cancellation.attempts[c.RequestID] = attempt
			stopCtx, cancel := context.WithTimeout(ctx, runtime.Timeout)
			go func() {
				defer close(attempt.done)
				defer cancel()
				var children sync.WaitGroup
				var outcomes sync.Mutex
				children.Go(func() {
					err := e.stopTaskWork(stopCtx, c.Target.TaskID)
					outcomes.Lock()
					attempt.err = errors.Join(attempt.err, err)
					outcomes.Unlock()
				})
				for _, owned := range runs {
					children.Go(func() {
						err := runtime.StopRun(stopCtx, owned.run, owned.reviews)
						outcomes.Lock()
						attempt.err = errors.Join(attempt.err, err)
						outcomes.Unlock()
					})
				}
				children.Wait()
				// A call registered before Stop may have entered its concrete
				// provider while the first inventory was read. Re-observe after
				// every local launch path has joined and cannot create more work.
				if attempt.err == nil {
					for _, owned := range runs {
						attempt.err = errors.Join(attempt.err, runtime.StopRun(stopCtx, owned.run, owned.reviews))
					}
				}
			}()
		}
		select {
		case <-attempt.done:
			delete(e.cancellation.attempts, c.RequestID)
			if attempt.err != nil && e.logger != nil {
				e.logger.Warn("task stop proof failed", "request", c.RequestID, "error", attempt.err)
			}
			if err := e.acknowledgeTaskStop(ctx, *c, attempt.err == nil); err != nil {
				return err
			}
		default:
			if !time.Now().Before(attempt.deadline) {
				if err := e.acknowledgeTaskStop(ctx, *c, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (e *Engine) cancellationRuns(ctx context.Context, cancellation domain.TaskCancellation) ([]cancellationRun, error) {
	var runs []cancellationRun
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		current, err := tx.GetTaskCancellation(ctx, cancellation.RequestID)
		if err != nil {
			return err
		}
		if current.TargetDigest != cancellation.TargetDigest {
			return store.ErrCancellationBinding
		}
		for _, member := range current.Target.Runs {
			run, err := tx.GetRun(ctx, member.RunID)
			if err != nil {
				return err
			}
			if run.TaskID != current.Target.TaskID || run.ProjectID != current.Target.ProjectID {
				return domain.ErrParentKeyMismatch
			}
			reviews, err := tx.ListReviewRequests(ctx, run.ID)
			if err != nil {
				return err
			}
			runs = append(runs, cancellationRun{run: run, reviews: reviews})
		}
		return nil
	})
	return runs, err
}

func (e *Engine) acknowledgeTaskStop(ctx context.Context, c domain.TaskCancellation, stopped bool) error {
	state := domain.TaskCancellationFailed
	if stopped {
		state = domain.TaskCancellationConfirmed
	}
	if c.State == state {
		return nil
	}
	evidence, err := json.Marshal(struct {
		Version   string
		Request   string
		Target    domain.Digest
		State     domain.TaskCancellationState
		Inventory domain.TaskCancellationTarget
	}{"freeside.task-runtime-stop/v1", c.RequestID, c.TargetDigest, state, c.Target})
	if err != nil {
		return err
	}
	digest := domain.Digest(contentaddr.Sum(evidence))
	if e.artifacts != nil {
		if _, err := e.artifacts.Put(digest, bytes.NewReader(evidence)); err != nil {
			return err
		}
	}
	ack := domain.TaskCancellationAcknowledgement{
		ID: fmt.Sprintf("runtime-%s-%s", c.RequestID, state), RequestID: c.RequestID,
		TargetDigest: c.TargetDigest, State: state,
		EvidenceDigest: digest, RecordedAt: time.Now().UTC(),
	}
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		written, err := tx.AcknowledgeTaskCancellation(ctx, ack)
		if err == nil && !written {
			return errReplay
		}
		return err
	})
	if errors.Is(err, errReplay) || errors.Is(err, store.ErrCancellationBinding) {
		// A changed episode or epoch cannot inherit this worker's proof.
		return nil
	}
	return err
}
