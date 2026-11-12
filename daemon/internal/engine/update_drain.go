package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// UpdateDrain writes the update drain log (issue #3104): a request from the
// operator's tooling, its cancellation, and the lift a new build makes at
// start. Each write is one client-visible Write, so the sync revision moves
// and a client sees the drain stop appear or end. Each is safe to repeat
// after a crash on either side of its write: before it there is no row, so a
// repeat writes it; after it the row is there, so a repeat writes nothing.
//
// The log holds work and grants nothing, so its rows carry no command
// binding. The control socket's peer check is what limits who requests one.
type UpdateDrain struct {
	Store *store.Store
	// Build is the digest of this daemon's executable, recorded on each row.
	Build domain.Digest
	// Now defaults to the wall clock.
	Now func() time.Time
	// TransitionHook is the crash-matrix seam; production leaves it nil.
	TransitionHook DurableTransitionHook
}

// Request puts a drain in force and returns it. With a drain already in
// force it writes nothing and returns that drain, so its start time stays.
func (d UpdateDrain) Request(ctx context.Context) (domain.UpdateDrainTransition, error) {
	var drain domain.UpdateDrainTransition
	err := d.write(ctx, DurableTransitionUpdateDrainRequest, func(tx *store.WriteTx) (bool, error) {
		existing, err := tx.UpdateDrainInForce(ctx)
		if err != nil || existing != nil {
			if existing != nil {
				drain = *existing
			}
			return false, err
		}
		drain = d.transition(domain.UpdateDrainDraining)
		return true, tx.RecordUpdateDrainTransition(ctx, drain)
	})
	return drain, err
}

// Force puts a forced drain in force, then runs cancel to end the work the
// drain would otherwise wait for (issue #3105). Over no drain or a plain
// drain it appends one forced draining row; over a forced drain it writes
// nothing, so its start time stays. A plain Request over a forced drain
// writes nothing either. cancel runs on every call, so a repeat reaches work
// admitted since the last one.
//
// The row is a request write, under the request's crash seams. The
// cancellation has its own: a crash before it leaves this build's drain in
// force across the restart, and the restart itself ends the running work.
func (d UpdateDrain) Force(
	ctx context.Context, cancel func(context.Context) error,
) (domain.UpdateDrainTransition, error) {
	var drain domain.UpdateDrainTransition
	err := d.write(ctx, DurableTransitionUpdateDrainRequest, func(tx *store.WriteTx) (bool, error) {
		existing, err := tx.UpdateDrainInForce(ctx)
		if err != nil || (existing != nil && existing.Forced) {
			if existing != nil {
				drain = *existing
			}
			return false, err
		}
		drain = d.transition(domain.UpdateDrainDraining)
		drain.Forced = true
		return true, tx.RecordUpdateDrainTransition(ctx, drain)
	})
	if err != nil {
		return domain.UpdateDrainTransition{}, err
	}
	if err := runDurableTransitionHook(d.TransitionHook,
		DurableTransitionUpdateDrainForce, DurableTransitionBefore); err != nil {
		return domain.UpdateDrainTransition{}, err
	}
	if err := cancel(ctx); err != nil {
		return domain.UpdateDrainTransition{}, err
	}
	if err := runDurableTransitionHook(d.TransitionHook,
		DurableTransitionUpdateDrainForce, DurableTransitionAfter); err != nil {
		return domain.UpdateDrainTransition{}, err
	}
	return drain, nil
}

// CancelRootImplementations cancels every production run's root
// implementation invocation that has started and has no recorded terminal,
// through the stage driver's ordinary Cancel. It is a forced drain's
// cancellation: each canceled terminal then gets an uncounted daemon re-run
// (daemonRerunReason), which the drain holds until it lifts. Every other
// launch finishes as it does under a plain drain.
//
// An invocation the driver has no intent for yet has nothing to cancel; it is
// logged and skipped, and a repeated force reaches it once it starts. A
// marker that does not decode, and any other refusal (such as a quarantined
// stage intent, whose writer the daemon cannot reach), is returned after
// every other cancellation has been tried, so one bad record never shields
// the rest.
func (e *Engine) CancelRootImplementations(ctx context.Context) error {
	var (
		running    []domain.InvocationID
		unreadable error
	)
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		running, unreadable = nil, nil
		entries, err := tx.ListDispatchedOutbox(ctx, KindProductionInvocationRequested)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			request, err := decodeProductionRequest(entry)
			if err != nil {
				unreadable = errors.Join(unreadable, fmt.Errorf("production intent %q: %w", entry.IdempotencyKey, err))
				continue
			}
			if request.InvocationID != productionInvocationID(request.RunID) {
				continue
			}
			if _, err := tx.GetInbox(ctx, string(request.InvocationID)); err == nil {
				continue
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			running = append(running, request.InvocationID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("list root implementations: %w", err)
	}
	var (
		cancels  sync.WaitGroup
		mu       sync.Mutex
		canceled error
	)
	for _, id := range running {
		cancels.Go(func() {
			err := e.driver.Cancel(ctx, id)
			if errors.Is(err, exec.ErrUnknownInvocation) {
				e.logger.Info("forced drain skipped an invocation with no stage intent", "invocation", id)
				return
			}
			if err != nil {
				mu.Lock()
				canceled = errors.Join(canceled, fmt.Errorf("cancel %q: %w", id, err))
				mu.Unlock()
			}
		})
	}
	cancels.Wait()
	return errors.Join(unreadable, canceled)
}

// Cancel lifts the drain in force and reports whether there was one. It
// leaves an operator stop and every blocking item as they are.
func (d UpdateDrain) Cancel(ctx context.Context) (bool, error) {
	var lifted bool
	err := d.write(ctx, DurableTransitionUpdateDrainCancel, func(tx *store.WriteTx) (bool, error) {
		existing, err := tx.UpdateDrainInForce(ctx)
		if err != nil || existing == nil {
			return false, err
		}
		lifted = true
		return true, d.lift(ctx, tx)
	})
	return lifted, err
}

// LiftForBuild lifts a drain a different build requested, and reports
// whether it did. A drain this build requested stays: a daemon that crashed
// in the middle of a drain must not resume launches in the middle of the
// swap the drain was for. Start runs it before anything can launch.
func (d UpdateDrain) LiftForBuild(ctx context.Context) (bool, error) {
	var lifted bool
	err := d.write(ctx, DurableTransitionUpdateDrainLift, func(tx *store.WriteTx) (bool, error) {
		existing, err := tx.UpdateDrainInForce(ctx)
		if err != nil || existing == nil || existing.Build == d.Build {
			return false, err
		}
		lifted = true
		return true, d.lift(ctx, tx)
	})
	return lifted, err
}

// lift records the lifted row and ends every run's update_drain hold, which
// no longer describes a wait.
func (d UpdateDrain) lift(ctx context.Context, tx *store.WriteTx) error {
	if err := tx.RecordUpdateDrainTransition(ctx, d.transition(domain.UpdateDrainLifted)); err != nil {
		return err
	}
	return tx.ClearUpdateDrainHolds(ctx)
}

func (d UpdateDrain) transition(state domain.UpdateDrainState) domain.UpdateDrainTransition {
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	return domain.UpdateDrainTransition{State: state, Build: d.Build, OccurredAt: now().UTC()}
}

// errNoDrainWrite rolls back a Write that found nothing to record, so a
// repeated request or a cancel with no drain moves no revision.
var errNoDrainWrite = errors.New("no update drain write")

// write runs decide in one Write between the transition's crash seams.
// decide reports whether it recorded a row; when it did not, the Write rolls
// back and the after seam does not run.
func (d UpdateDrain) write(
	ctx context.Context, transition DurableTransition, decide func(*store.WriteTx) (bool, error),
) error {
	if err := runDurableTransitionHook(d.TransitionHook, transition, DurableTransitionBefore); err != nil {
		return err
	}
	err := d.Store.Write(ctx, func(tx *store.WriteTx) error {
		wrote, err := decide(tx)
		if err != nil {
			return err
		}
		if !wrote {
			return errNoDrainWrite
		}
		return nil
	})
	if errors.Is(err, errNoDrainWrite) {
		return nil
	}
	if err != nil {
		return err
	}
	return runDurableTransitionHook(d.TransitionHook, transition, DurableTransitionAfter)
}
