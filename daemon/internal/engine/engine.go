package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrInvocationLost means a recorded attempt survived but its driver session
// ended before a result was committed. Reconciliation preserves the attempt
// and fails loudly; retry policy and the execution-failure item belong to the
// later real-work workflow, not the 1A.0 walking skeleton.
var ErrInvocationLost = errors.New("invocation ended without an accepted result")

// ErrInvocationUnsuccessful means the driver committed a failed or canceled
// terminal result. The 1A.0 skeleton has no retry/failure-item policy, so it
// preserves the attempt and fails instead of laundering failure into an agent
// reply and advancing the workflow.
var ErrInvocationUnsuccessful = errors.New("invocation did not complete successfully")

// errForeignWorkflow marks durable invocation state owned by another workflow
// in the shared store. Selection skips it without consuming its outbox row;
// malformed state for an owned fake run remains a loud error.
var errForeignWorkflow = errors.New("invocation belongs to another workflow")

// errReplay rolls back a Write whose durable transition already exists. A
// successful no-op callback would still increment the server revision, so an
// idempotent engine pass must leave through an error and translate it here.
var errReplay = errors.New("engine transition already committed")

// Engine is the durable outer loop over the store, attention service, and one
// execution driver. It is safe to call Reconcile repeatedly; the store ledger
// and deterministic workflow identities collapse retries onto prior work.
type Engine struct {
	store                 *store.Store
	signet                *signet.Service
	driver                exec.StageDriver
	publication           *fakePublicationWorkflow
	fakePublicationPolicy *fakePublicationPolicyRecovery
	productionPublication *productionPublicationWorkflow
	elaboration           *elaborationWorkflow
	inference             *inference.Client
	// admission is the configured capability gate and durable-record writer
	// (see WithAdmission); nil leaves dispatch exactly as it was before a
	// runner backend existed to admit against.
	admission *admitter
	// derive supplies the per-attempt workspace and base when configured
	// (see WithAdmissionDerivation); nil keeps the static environment.
	derive AdmissionDerivation
	// pace bounds the per-pass observation writes (issue #394); process
	// state only, never authority.
	pace observationPace
	// logger reports what the reconcile loops do. Never nil after New, so
	// the loops log without checking.
	logger *slog.Logger
}

// Option configures an optional engine workflow without changing the shared
// store, signet, or driver contracts.
type Option func(*Engine) error

// WithLogger installs the reconcile loops' logger. Without it the engine
// discards its records, which is what every test wants and no unattended
// daemon does.
func WithLogger(logger *slog.Logger) Option {
	return func(e *Engine) error {
		if logger == nil {
			return errors.New("engine logger: nil logger")
		}
		e.logger = logger.With("subsystem", "engine")
		return nil
	}
}

// WithInference installs the daemon-side judgment-call boundary. It is
// optional so a daemon with inference unavailable remains fully operable.
func WithInference(client *inference.Client) Option {
	return func(e *Engine) error {
		if client == nil {
			return errors.New("engine inference: nil client")
		}
		e.inference = client
		return nil
	}
}

// New constructs an Engine from already-open boundaries. Their lifetimes stay
// with the daemon composition that supplied them.
func New(st *store.Store, attention *signet.Service, driver exec.StageDriver, opts ...Option) (*Engine, error) {
	if st == nil {
		return nil, errors.New("new engine: nil store")
	}
	if attention == nil {
		return nil, errors.New("new engine: nil signet service")
	}
	if driver == nil {
		return nil, errors.New("new engine: nil stage driver")
	}
	e := &Engine{
		store: st, signet: attention, driver: driver,
		fakePublicationPolicy: &fakePublicationPolicyRecovery{store: st},
		logger:                slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("new engine: nil option")
		}
		if err := opt(e); err != nil {
			return nil, err
		}
	}
	if e.productionPublication != nil {
		e.productionPublication.inference = e.inference
	}
	return e, nil
}

// ReconcileResult reports the work one pass committed. It is operational
// evidence for tests and the daemon loop, not workflow authority.
type ReconcileResult struct {
	RunTransitions            int
	InvocationsStarted        int
	ResultsAccepted           int
	PublicationTasksCompleted int
	ReadyCleanItemsCreated    int
	ReadyDegradedItemsCreated int
	// ReadyItemsCreated is the operational event count retained for existing
	// callers. Readiness authority is carried only by the two verdict-class
	// fields above and the persisted ReadinessVerdict.
	ReadyItemsCreated   int
	BlockedItemsCreated int
	LastPRNumber        int
}

// Reconcile advances every durable run and invocation as far as the currently
// observed state permits. It never waits for a driver: unstarted work remains
// in the outbox, while started work remains in the Run attempt history for a
// later pass.
//
// The production publication lane is deliberately absent: its external
// workflow blocks for minutes, so it advances on its own loop (see
// ReconcileProductionPublications) instead of stalling every run, invocation,
// and attention item behind one verification.
func (e *Engine) Reconcile(ctx context.Context) (ReconcileResult, error) {
	if e.inference != nil {
		_ = e.inference.Maintain(ctx)
	}
	if err := e.ConvergeLegacyFakePublicationPolicies(ctx); err != nil {
		return ReconcileResult{}, fmt.Errorf("converge legacy fake-publication policies: %w", err)
	}
	runTransitions, err := e.reconcileRuns(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile runs: %w", err)
	}
	started, accepted, err := e.reconcileInvocations(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile invocations: %w", err)
	}
	result := ReconcileResult{
		RunTransitions:     runTransitions,
		InvocationsStarted: started,
		ResultsAccepted:    accepted,
	}
	if e.elaboration != nil {
		startedRuns, blocked, gateErr := e.reconcileElaborationGates(ctx)
		result.RunTransitions += startedRuns
		result.BlockedItemsCreated += blocked
		if gateErr != nil {
			return result, fmt.Errorf("reconcile elaboration gates: %w", gateErr)
		}
	}
	if e.publication != nil {
		publication, err := e.ReconcileFakePublications(ctx)
		result.PublicationTasksCompleted += publication.PublicationTasksCompleted
		result.ReadyItemsCreated += publication.ReadyItemsCreated
		result.BlockedItemsCreated += publication.BlockedItemsCreated
		if publication.LastPRNumber > 0 {
			result.LastPRNumber = publication.LastPRNumber
		}
		return result, err
	}
	return result, nil
}

// ReconcileProductionPublications advances only the production publication
// lane: it clones the exact base, replays the export, verifies in a container,
// and calls GitHub, so one pass can block for minutes at an external boundary.
// Keeping it off Reconcile is what lets the rest of the engine keep its
// cadence while a publication is stuck (issue #425). Per-task serialization is
// unchanged: the lane holds its own mutex, and each task still takes its
// on-disk lock.
func (e *Engine) ReconcileProductionPublications(ctx context.Context) (ReconcileResult, error) {
	if e.productionPublication == nil {
		return ReconcileResult{}, errors.New("production publication workflow is not configured")
	}
	if e.inference != nil {
		_ = e.inference.Maintain(ctx)
	}
	publication, err := e.productionPublication.reconcile(ctx)
	result := ReconcileResult{
		ResultsAccepted:           publication.accepted,
		PublicationTasksCompleted: publication.completed,
		ReadyCleanItemsCreated:    publication.readyClean,
		ReadyDegradedItemsCreated: publication.readyDegraded,
		ReadyItemsCreated:         publication.readyClean + publication.readyDegraded,
		BlockedItemsCreated:       publication.blocked,
		LastPRNumber:              publication.lastPR,
	}
	if err != nil {
		return result, fmt.Errorf("reconcile production publications: %w", err)
	}
	return result, nil
}

// ReconcileFakePublications advances only the attended fake-publication lane.
// One-shot publication commands use this entry point so an unrelated run or
// invocation in the shared store cannot be advanced by their private driver.
func (e *Engine) ReconcileFakePublications(ctx context.Context) (ReconcileResult, error) {
	if e.publication == nil {
		return ReconcileResult{}, errors.New("fake publication workflow is not configured")
	}
	if err := e.ConvergeLegacyFakePublicationPolicies(ctx); err != nil {
		return ReconcileResult{}, fmt.Errorf("converge legacy fake-publication policies: %w", err)
	}
	publication, err := e.publication.reconcile(ctx)
	result := ReconcileResult{
		PublicationTasksCompleted: publication.completed,
		ReadyItemsCreated:         publication.ready,
		BlockedItemsCreated:       publication.blocked,
		LastPRNumber:              publication.lastPRNumber,
	}
	if err != nil {
		return result, fmt.Errorf("reconcile fake publications: %w", err)
	}
	return result, nil
}

// ReconcileFakePublication advances only the requested attended publication.
// A one-shot command uses this form so a broken sibling cannot terminate or
// otherwise interfere with the requested run.
func (e *Engine) ReconcileFakePublication(
	ctx context.Context,
	runID domain.RunID,
) (ReconcileResult, error) {
	if e.publication == nil {
		return ReconcileResult{}, errors.New("fake publication workflow is not configured")
	}
	publication, err := e.publication.reconcileRun(ctx, runID)
	result := ReconcileResult{
		PublicationTasksCompleted: publication.completed,
		ReadyItemsCreated:         publication.ready,
		BlockedItemsCreated:       publication.blocked,
		LastPRNumber:              publication.lastPRNumber,
	}
	if err != nil {
		return result, fmt.Errorf("reconcile fake publication %q: %w", runID, err)
	}
	return result, nil
}

// Run reconciles immediately and then on interval until ctx is canceled. A
// correctness error stops the loop instead of being hidden by retries; a
// caller may restart after repairing the durable state or driver boundary.
//
// It does not advance the production publication lane; a composition that
// configured one runs RunProductionPublications beside this loop.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	return runReconcileLoop(ctx, e.logger, "run engine", interval, func(ctx context.Context) error {
		_, err := e.Reconcile(ctx)
		return err
	})
}

// RunProductionPublications advances the production publication lane on its
// own cadence until ctx is canceled. It is a separate loop from Run because a
// single task holds an external boundary (clone, containerized verification,
// GitHub) for minutes, and inside Run that would stall every unrelated run,
// invocation, and attention item for the same span (issue #425).
//
// Cancellation stops the loop where Run's does: the in-flight task's own
// context is canceled with it, so the caller's wait ends once the boundary
// returns and the durable task stays pending for the next process.
func (e *Engine) RunProductionPublications(ctx context.Context, interval time.Duration) error {
	if e.productionPublication == nil {
		return errors.New("production publication workflow is not configured")
	}
	return runReconcileLoop(ctx, e.logger, "run production publications", interval,
		func(ctx context.Context) error {
			_, err := e.ReconcileProductionPublications(ctx)
			return err
		})
}

// runReconcileLoop runs pass immediately and then on interval until ctx is
// canceled. A correctness error stops the loop instead of being hidden by
// retries; cancellation during a pass is shutdown, not a failure.
//
// It is the single logging seam for both engine loops. A stopping error is
// recorded here, before it propagates to a caller that may only hand it to
// a channel, so the reason the loop died is legible without the daemon
// having to survive to report it. Completed passes are debug: this runs on
// a fixed cadence forever, and an info record per pass is a log the
// operator stops reading.
func runReconcileLoop(
	ctx context.Context,
	logger *slog.Logger,
	name string,
	interval time.Duration,
	pass func(context.Context) error,
) error {
	if interval <= 0 {
		return fmt.Errorf("%s: interval %s must be positive", name, interval)
	}
	logger = logger.With("loop", name)
	logger.Info("reconcile loop started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := pass(ctx); err != nil {
			if ctx.Err() != nil {
				logger.Info("reconcile loop stopped during shutdown")
				return nil
			}
			logger.Error("reconcile pass failed", "error", err)
			return err
		}
		logger.Debug("reconcile pass complete")
		select {
		case <-ctx.Done():
			logger.Info("reconcile loop stopped")
			return nil
		case <-ticker.C:
		}
	}
}
