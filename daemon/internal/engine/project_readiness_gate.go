package engine

import (
	"context"
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ProjectReadinessGate refuses new work for a project that is not ready
// (#3205). It runs in the caller's transaction and returns an error that
// unwraps to domain.ErrProjectNotReady, carrying a
// *domain.ProjectNotReadyError, when the project may not take new work; any
// other error is a store failure. The daemon composes it beside its
// readiness evaluator (daemoncompose); a nil gate checks nothing, so fake,
// scenario, and driverless daemons, and a daemon composed for no project,
// behave as they did before.
//
// The gate decides only whether new work starts: a new task or run from a
// submission, label intake, and the three re-entry cycles. Work on an
// existing run ends in an admitted invocation, which the project_not_ready
// card already holds through the operating gate.
type ProjectReadinessGate func(ctx context.Context, tx *store.ReadTx, project domain.ProjectID) error

// requireProjectReady runs gate for project in tx, passing when gate is
// nil. Every engine path that starts new work calls it before its first
// write (TestEveryNewWorkPathChecksProjectReadiness).
func requireProjectReady(
	ctx context.Context, gate ProjectReadinessGate, tx *store.ReadTx, project domain.ProjectID,
) error {
	if gate == nil {
		return nil
	}
	return gate(ctx, tx, project)
}

// waiveWriterImage is gate, except that a project whose only failed check
// is writer_image passes. The image-repair task uses it: that task is
// opened because the project's image cannot be built, which is exactly
// what writer_image reports, so refusing it would refuse the repair. Its
// run is still held at admission while the card stays open.
func waiveWriterImage(gate ProjectReadinessGate) ProjectReadinessGate {
	if gate == nil {
		return nil
	}
	return func(ctx context.Context, tx *store.ReadTx, project domain.ProjectID) error {
		err := gate(ctx, tx, project)
		var notReady *domain.ProjectNotReadyError
		if errors.As(err, &notReady) && notReady.OnlyNotPassed(domain.ProjectReadinessWriterImage) {
			return nil
		}
		return err
	}
}

// WithProjectReadinessGate installs the daemon's project readiness gate. The
// engine applies it where its own workers start new work: the readiness
// re-entry a moved head starts while a cycle runs.
func WithProjectReadinessGate(gate ProjectReadinessGate) Option {
	return func(e *Engine) error {
		if gate == nil {
			return errors.New("engine project readiness gate: nil function")
		}
		e.readinessGate = gate
		return nil
	}
}
