package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
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
	// Unprovable is optional. A non-empty reason says no attempt can ever
	// prove a stop of the task, so one that has reached failed_to_stop is not
	// retried; the reason must hold for as long as the state root does. An
	// error says only that the check failed and leaves the stop retryable. A
	// nil hook keeps every failed stop retryable.
	Unprovable func(context.Context, domain.TaskID) (reason string, err error)
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
	retries  map[string]*taskStopRetry
}

// taskStopRetry paces one open stop request for the life of this process.
// Nothing here is durable: a restarted daemon rebuilds it from the request's
// recorded state and the runtime's Unprovable answer.
type taskStopRetry struct {
	failures  int
	notBefore time.Time
	logged    string // key of the last failure written, so a repeat stays quiet
	settled   bool   // no further attempt or log line in this process
}

// taskStopRetryDelay is the schedule reviewRetryDelay uses: one second,
// doubling per consecutive failure, capped at 2^8 seconds.
func taskStopRetryDelay(failures int) time.Duration {
	return time.Second << min(failures-1, 8)
}

// taskStopFailureKey identifies a failure for log deduplication. Children
// stop concurrently and their errors join in completion order, so the same
// failures can arrive in a different order on each attempt.
func taskStopFailureKey(failure error) string {
	lines := strings.Split(failure.Error(), "\n")
	slices.Sort(lines)
	return strings.Join(lines, "\n")
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
// passes cannot accumulate duplicate stop attempts. A failed stop is retried
// with backoff, and one that can never be proven is settled after its first
// failure instead of retried.
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
	if e.cancellation.retries == nil {
		e.cancellation.retries = make(map[string]*taskStopRetry)
	}
	open := make(map[string]struct{}, len(e.cancellation.retries))
	for _, snapshot := range tasks {
		c := snapshot.Value.Cancellation
		if c == nil || c.State == domain.TaskCancellationConfirmed {
			continue
		}
		// Runs for a settled request too: after a restart this is what keeps
		// this process from launching work for the fenced task.
		e.taskExecution(c.Target.TaskID).cancel()
		open[c.RequestID] = struct{}{}
		retry := e.cancellation.retries[c.RequestID]
		if retry == nil {
			retry = &taskStopRetry{}
			e.cancellation.retries[c.RequestID] = retry
		}
		attempt := e.cancellation.attempts[c.RequestID]
		if attempt == nil {
			if retry.settled || time.Now().Before(retry.notBefore) {
				continue
			}
			runtime := e.cancellation.runtime
			if c.State == domain.TaskCancellationFailed && runtime.Unprovable != nil {
				// An earlier attempt, usually an earlier process's, already
				// recorded the failure. When no attempt can prove the stop,
				// another only repeats the refusal. A failed check is not an
				// answer, so the attempt goes ahead.
				if reason, err := runtime.Unprovable(ctx, c.Target.TaskID); err == nil && reason != "" {
					e.settleTaskStop(retry, "task stop cannot be proven; not retrying",
						"request", c.RequestID, "reason", reason)
					continue
				}
			}
			runs, err := e.cancellationRuns(ctx, *c)
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				if err := e.acknowledgeTaskStop(ctx, retry, *c, true); err != nil {
					return err
				}
				continue
			}
			if runtime.StopRun == nil {
				if err := e.acknowledgeTaskStop(ctx, retry, *c, false); err != nil {
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
			if err := e.acknowledgeTaskStop(ctx, retry, *c, attempt.err == nil); err != nil {
				return err
			}
			// A refused acknowledgement settled the request above.
			if attempt.err != nil && !retry.settled {
				e.deferTaskStop(ctx, retry, *c, attempt.err)
			}
		default:
			// A refusal at the deadline settled the request, so later passes
			// only wait for the worker to return.
			if !retry.settled && !time.Now().Before(attempt.deadline) {
				if err := e.acknowledgeTaskStop(ctx, retry, *c, false); err != nil {
					return err
				}
			}
		}
	}
	// A confirmed or vanished request needs no pacing, and dropping it bounds
	// the map by the requests still open.
	maps.DeleteFunc(e.cancellation.retries, func(id string, _ *taskStopRetry) bool {
		_, ok := open[id]
		return !ok
	})
	return nil
}

// settleTaskStop ends attempts on a request for the life of this process and
// writes the one line that says so.
func (e *Engine) settleTaskStop(retry *taskStopRetry, msg string, args ...any) {
	if retry.settled {
		return
	}
	retry.settled = true
	if e.logger != nil {
		e.logger.Warn(msg, args...)
	}
}

// deferTaskStop handles an attempt that failed and was recorded as
// failed_to_stop. A stop the runtime can never prove is settled; any other
// failure may clear, so it backs off and is logged only when its text changes.
func (e *Engine) deferTaskStop(ctx context.Context, retry *taskStopRetry, c domain.TaskCancellation, failure error) {
	if unprovable := e.cancellation.runtime.Unprovable; unprovable != nil {
		reason, err := unprovable(ctx, c.Target.TaskID)
		if err == nil && reason != "" {
			e.settleTaskStop(retry, "task stop cannot be proven; not retrying",
				"request", c.RequestID, "reason", reason, "error", failure)
			return
		}
		if err != nil {
			failure = errors.Join(failure, fmt.Errorf("unprovable check: %w", err))
		}
	}
	retry.failures++
	delay := taskStopRetryDelay(retry.failures)
	retry.notBefore = time.Now().Add(delay)
	if key := taskStopFailureKey(failure); key != retry.logged {
		retry.logged = key
		if e.logger != nil {
			e.logger.Warn("task stop proof failed", "request", c.RequestID, "error", failure, "retry_in", delay)
		}
	}
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

// acknowledgeTaskStop records an attempt's outcome. The store refuses it with
// store.ErrCancellationBinding when the task's episode, run membership, or
// sync epoch changed after the Stop was accepted. None of those changes back,
// so a refusal settles the request instead of leaving it to be retried.
func (e *Engine) acknowledgeTaskStop(ctx context.Context, retry *taskStopRetry, c domain.TaskCancellation, stopped bool) error {
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
	if errors.Is(err, store.ErrCancellationBinding) {
		// A changed episode or epoch cannot inherit this worker's proof.
		e.settleTaskStop(retry, "task stop acknowledgement refused; not retrying in this process",
			"request", c.RequestID, "state", state)
		return nil
	}
	if errors.Is(err, errReplay) {
		return nil
	}
	return err
}
