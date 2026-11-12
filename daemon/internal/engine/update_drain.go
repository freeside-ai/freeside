package engine

import (
	"context"
	"errors"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
