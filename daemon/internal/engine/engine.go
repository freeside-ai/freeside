package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
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
	store       *store.Store
	signet      *signet.Service
	driver      exec.StageDriver
	publication *fakePublicationWorkflow
}

// Option configures an optional engine workflow without changing the shared
// store, signet, or driver contracts.
type Option func(*Engine) error

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
	e := &Engine{store: st, signet: attention, driver: driver}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("new engine: nil option")
		}
		if err := opt(e); err != nil {
			return nil, err
		}
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
	ReadyItemsCreated         int
	BlockedItemsCreated       int
	LastPRNumber              int
}

// Reconcile advances every durable run and invocation as far as the currently
// observed state permits. It never waits for a driver: unstarted work remains
// in the outbox, while started work remains in the Run attempt history for a
// later pass.
func (e *Engine) Reconcile(ctx context.Context) (ReconcileResult, error) {
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
	if e.publication == nil {
		return result, nil
	}
	publication, err := e.ReconcileFakePublications(ctx)
	result.PublicationTasksCompleted = publication.PublicationTasksCompleted
	result.ReadyItemsCreated = publication.ReadyItemsCreated
	result.BlockedItemsCreated = publication.BlockedItemsCreated
	result.LastPRNumber = publication.LastPRNumber
	return result, err
}

// ReconcileFakePublications advances only the attended fake-publication lane.
// One-shot publication commands use this entry point so an unrelated run or
// invocation in the shared store cannot be advanced by their private driver.
func (e *Engine) ReconcileFakePublications(ctx context.Context) (ReconcileResult, error) {
	if e.publication == nil {
		return ReconcileResult{}, errors.New("fake publication workflow is not configured")
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

// Run reconciles immediately and then on interval until ctx is canceled. A
// correctness error stops the loop instead of being hidden by retries; a
// caller may restart after repairing the durable state or driver boundary.
func (e *Engine) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("run engine: interval %s must be positive", interval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := e.Reconcile(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
