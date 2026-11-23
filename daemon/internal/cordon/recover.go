package cordon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Restart recovery (#3468). A launch's supervisor outlives the daemon, and
// everything a later daemon needs is in the run's directory, so it can act
// on a run by its ID alone. Each method here mirrors a stage.Gate method
// (HandoffStarted, RequestCancellation) so the driver port can wrap it.
// Nothing here reads a path from a record: the run's directory follows from
// RunsRoot and the run ID, and a process is signaled only while its pid
// still has the start time its claim names.

var (
	// ErrNoRun means the run has no directory: it was never claimed.
	ErrNoRun = errors.New("cordon: no such run")
	// ErrDetached is a Launch that returned because Detach ran, or that
	// Detach refused. The run, if any, keeps running.
	ErrDetached = errors.New("cordon: the backend is detached")
)

// Detach makes every waiting Launch return ErrDetached at once,
// signaling nothing, and every later one refuse: the daemon is exiting and
// leaves its runs to their supervisors, for the next daemon to recover. A
// daemon shutting down calls it before it cancels any Launch's context,
// since that cancellation cancels the run for good.
func (b *Backend) Detach() {
	if b.detached != nil {
		b.detachOnce.Do(func() { close(b.detached) })
	}
}

func (b *Backend) attached() error {
	select {
	case <-b.detached:
		return ErrDetached
	default:
		return nil
	}
}

// Started reports whether runID's directory exists: a launch claims it
// before it starts anything.
func (b *Backend) Started(ctx context.Context, runID string) (bool, error) {
	_, err := b.existingRun(ctx, runID)
	if errors.Is(err, ErrNoRun) {
		return false, nil
	}
	return err == nil, err
}

// existingRun returns runID's layout once its directory exists.
func (b *Backend) existingRun(ctx context.Context, runID string) (layout, error) {
	if err := ctx.Err(); err != nil {
		return layout{}, err
	}
	if !plainRunID(runID) {
		return layout{}, fmt.Errorf("cordon: run id %q must be one plain path component", runID)
	}
	l := layoutFor(b.cfg.RunsRoot, runID)
	if _, err := os.Lstat(l.dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return layout{}, fmt.Errorf("%w: %s", ErrNoRun, runID)
		}
		return layout{}, fmt.Errorf("cordon: inspect %s: %w", l.dir, err)
	}
	return l, nil
}

// RequestCancellation makes runID end canceled and never export. It writes
// the durable marker first and then sends SIGTERM to the run's supervisor,
// if its claim names one still alive with its start time, which ends the
// agent as a canceled Launch does. A supervisor that claims later finds the
// marker after its claim, so one of the two always sees the request.
func (b *Backend) RequestCancellation(ctx context.Context, runID string) error {
	l, err := b.existingRun(ctx, runID)
	if err != nil {
		return err
	}
	err = writeRecord(l.cancel, cancelMarker{Version: cancelVersion, RunID: runID})
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	claim, err := readClaim(l, runID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || claim.Supervisor == nil {
		return err
	}
	host, err := b.newHost()
	if err != nil {
		return err
	}
	alive, err := host.same(claim.Supervisor.hostProcess())
	if err != nil || !alive {
		return err
	}
	return host.terminate(claim.Supervisor.PID)
}
