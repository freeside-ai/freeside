package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Production workflow lane (#237, plan §5.12): a run created by `freesided
// submit` from an operator-approved, digest-addressed specification, executed
// unattended by the real stage driver. The lane shares the store, admission,
// and driver contracts with the 1A.0 walking skeleton but binds its
// invocation to input artifacts alone (ConversationID nil, the shape
// domain.AgentInvocation reserves for exactly this), so no conversation or
// attention item participates in dispatch.
//
// Ownership marker: a Run carries no workflow-kind discriminator, so each
// lane needs its own concrete marker (see reconcileRuns). The production
// marker is the deterministic invocation-request outbox row committed in the
// same transaction as the run: its idempotency key is the lane's derived
// invocation id and its kind is KindProductionInvocationRequested, so a
// foreign run can never acquire one retroactively and a submit replay
// converges on it.

// KindProductionInvocationRequested is the outbox kind for the production
// lane's dispatch intent. Exported with its backup-payload extractor so the
// daemon composition can register it (store.BackupPayloadDigestExtractor).
const KindProductionInvocationRequested = "production_invocation_requested"

// kindProductionStageTerminal is the inbox kind recording the engine's
// at-most-once acceptance of a production stage's terminal outcome.
const kindProductionStageTerminal = "production_stage_terminal"

// productionStageName is the single Phase 1A.2 implement stage (§11 1A.2).
const productionStageName = "implement"

// productionSpecificationArtifactType is the exact role registered by
// freesided submit for the one invocation-owned production input.
const productionSpecificationArtifactType = "specification"

// productionPolicyArtifactType is the exact role registered by freesided
// submit for the canonical resolved-policy bytes.
const productionPolicyArtifactType = "policy"

func productionStageID(runID domain.RunID) domain.StageID {
	return domain.StageID("implement-" + string(runID))
}

func productionInvocationID(runID domain.RunID) domain.InvocationID {
	return domain.InvocationID("inv-implement-" + string(runID))
}

// productionInvocationRequest is the outbox payload for one production
// dispatch intent. Unlike the conversation lane's request it carries the run
// and stage directly: there is no attention item to resolve them through.
type productionInvocationRequest struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        domain.RunID        `json:"run_id"`
	StageID      domain.StageID      `json:"stage_id"`
}

// ProductionRunSpec is the fixed input `freesided submit` registers: the
// operator-approved specification and resolved policy, already durable as
// digest-addressed artifacts (§5.12).
type ProductionRunSpec struct {
	RunID     domain.RunID
	ProjectID domain.ProjectID
	// SpecArtifactID and PolicyArtifactID name the pre-registered artifacts
	// whose digests become the run's SpecDigest and PolicyDigest. Submission
	// verifies the registration rather than trusting caller-supplied digests.
	SpecArtifactID domain.ArtifactID
	// PolicyArtifactID addresses the canonical ResolvedPolicy.Keys bytes;
	// ResolvedPolicy is persisted atomically with the run.
	PolicyArtifactID domain.ArtifactID
	ResolvedPolicy   domain.ResolvedPolicy
}

// ProductionRun reports the durable identities one submission converges on.
type ProductionRun struct {
	Run          domain.Run
	InvocationID domain.InvocationID
	StageID      domain.StageID
}

// SubmitProductionRun idempotently persists one production run with its
// implement stage, its artifact-bound invocation, and the dispatch intent the
// engine's reconcile loop picks up, all in one transaction. It is a package
// function over the store, not an Engine method: submission needs no driver
// or attention composition, so `freesided submit` can call it in-process.
//
// A replay converges on the stored state; a retry whose fixed bindings
// disagree with the stored run fails rather than retargeting it.
func SubmitProductionRun(ctx context.Context, st *store.Store, spec ProductionRunSpec) (ProductionRun, error) {
	if st == nil {
		return ProductionRun{}, errors.New("submit production run: nil store")
	}
	if spec.RunID == "" || spec.ProjectID == "" {
		return ProductionRun{}, fmt.Errorf("submit production run: run and project ids are required: %w", domain.ErrEmptyID)
	}
	if spec.SpecArtifactID == "" || spec.PolicyArtifactID == "" {
		return ProductionRun{}, fmt.Errorf("submit production run %q: spec and policy artifact ids are required: %w",
			spec.RunID, domain.ErrEmptyID)
	}
	if err := spec.ResolvedPolicy.Validate(); err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q resolved policy: %w", spec.RunID, err)
	}
	if spec.ResolvedPolicy.RunID != spec.RunID {
		return ProductionRun{}, fmt.Errorf(
			"submit production run %q resolved policy names run %q: %w",
			spec.RunID, spec.ResolvedPolicy.RunID, domain.ErrParentKeyMismatch)
	}

	stageID := productionStageID(spec.RunID)
	invocationID := productionInvocationID(spec.RunID)
	payload, err := json.Marshal(productionInvocationRequest{
		InvocationID: invocationID, RunID: spec.RunID, StageID: stageID,
	})
	if err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q: %w", spec.RunID, err)
	}
	invocation, err := domain.NewAgentInvocation(
		invocationID, []domain.ArtifactID{spec.SpecArtifactID}, nil, 0,
	)
	if err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q: %w", spec.RunID, err)
	}

	var (
		run        domain.Run
		runCreated bool
	)
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		// The digests come from the registered artifacts, not the caller: a
		// submission for bytes the store does not hold is refused, and the
		// run's trusted configuration is bound to what was actually
		// registered (§5.8).
		specArtifact, err := tx.GetArtifact(ctx, spec.SpecArtifactID)
		if err != nil {
			return fmt.Errorf("specification artifact %q: %w", spec.SpecArtifactID, err)
		}
		policyArtifact, err := tx.GetArtifact(ctx, spec.PolicyArtifactID)
		if err != nil {
			return fmt.Errorf("policy artifact %q: %w", spec.PolicyArtifactID, err)
		}
		if specArtifact.Type != productionSpecificationArtifactType {
			return fmt.Errorf(
				"specification artifact %q has type %q: %w",
				spec.SpecArtifactID, specArtifact.Type, domain.ErrParentKeyMismatch,
			)
		}
		if policyArtifact.Type != productionPolicyArtifactType {
			return fmt.Errorf(
				"policy artifact %q has type %q: %w",
				spec.PolicyArtifactID, policyArtifact.Type, domain.ErrParentKeyMismatch,
			)
		}
		if policyArtifact.Digest != spec.ResolvedPolicy.Digest {
			return fmt.Errorf(
				"policy artifact %q digest %q disagrees with resolved policy %q: %w",
				spec.PolicyArtifactID, policyArtifact.Digest,
				spec.ResolvedPolicy.Digest, domain.ErrParentKeyMismatch)
		}

		want := domain.Run{
			ID: spec.RunID, ProjectID: spec.ProjectID,
			SpecDigest: specArtifact.Digest, PolicyDigest: policyArtifact.Digest,
			Stages: []domain.Stage{{
				ID: stageID, RunID: spec.RunID,
				Name: productionStageName, Attempts: []domain.Attempt{},
			}},
		}
		if err := want.Validate(); err != nil {
			return err
		}

		existing, err := tx.GetRun(ctx, spec.RunID)
		switch {
		case err == nil:
			if existing.ProjectID != want.ProjectID || existing.SpecDigest != want.SpecDigest ||
				existing.PolicyDigest != want.PolicyDigest {
				return fmt.Errorf("fixed bindings disagree with stored run: %w", domain.ErrImmutableTransition)
			}
			if _, ok := findProductionStage(existing); !ok {
				return fmt.Errorf("stored run has no %q stage: %w", productionStageName, domain.ErrParentKeyMismatch)
			}
			// The marker is the production lane's ownership fact and must
			// already be part of an existing run's atomic creation. Creating
			// it here would let an explicit --run-id retroactively claim a
			// structurally compatible run from another workflow.
			entry, markerErr := tx.GetOutbox(ctx, string(invocationID))
			if markerErr != nil {
				if errors.Is(markerErr, store.ErrNotFound) {
					return fmt.Errorf(
						"stored run has no production ownership marker: %w",
						domain.ErrImmutableTransition,
					)
				}
				return markerErr
			}
			if entry.Kind != KindProductionInvocationRequested ||
				!bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf(
					"stored run has a foreign production marker: %w",
					domain.ErrImmutableTransition,
				)
			}
			storedPolicy, policyErr := tx.GetResolvedPolicy(ctx, spec.RunID)
			if policyErr != nil {
				if errors.Is(policyErr, store.ErrNotFound) {
					return fmt.Errorf(
						"stored production run has no resolved policy: %w",
						domain.ErrImmutableTransition,
					)
				}
				return policyErr
			}
			if storedPolicy.RunID != spec.ResolvedPolicy.RunID ||
				storedPolicy.Digest != spec.ResolvedPolicy.Digest ||
				!slices.Equal(storedPolicy.Keys, spec.ResolvedPolicy.Keys) {
				return fmt.Errorf(
					"stored production run has a foreign resolved policy: %w",
					domain.ErrImmutableTransition,
				)
			}
			storedInvocation, invocationErr := tx.GetAgentInvocation(ctx, invocationID)
			if invocationErr != nil {
				if errors.Is(invocationErr, store.ErrNotFound) {
					return fmt.Errorf(
						"stored production run has no agent invocation: %w",
						domain.ErrImmutableTransition,
					)
				}
				return invocationErr
			}
			if storedInvocation.ID != invocation.ID ||
				!slices.Equal(storedInvocation.InputIDs, invocation.InputIDs) ||
				storedInvocation.ConversationID != nil ||
				storedInvocation.ThroughSequence != invocation.ThroughSequence {
				return fmt.Errorf(
					"stored production run has a foreign agent invocation: %w",
					domain.ErrImmutableTransition,
				)
			}
			run = existing
		case errors.Is(err, store.ErrNotFound):
			if err := tx.PutRun(ctx, want); err != nil {
				return err
			}
			run = want
			runCreated = true
		default:
			return err
		}

		if runCreated {
			// A newly created production run must create its invocation and
			// policy and ownership marker in this transaction. Pre-existing
			// lane keys are not partial production state: they belong to
			// something else or are damaged, and adopting them would recreate
			// the same retroactive-claim bug under a different object.
			if _, err := tx.GetResolvedPolicy(ctx, spec.RunID); err == nil {
				return fmt.Errorf(
					"new production run's resolved policy already exists: %w",
					domain.ErrImmutableTransition,
				)
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if _, err := tx.GetAgentInvocation(ctx, invocationID); err == nil {
				return fmt.Errorf(
					"new production run's invocation already exists: %w",
					domain.ErrImmutableTransition,
				)
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if _, err := tx.GetOutbox(ctx, string(invocationID)); err == nil {
				return fmt.Errorf(
					"new production run's ownership marker already exists: %w",
					domain.ErrImmutableTransition,
				)
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := tx.PutResolvedPolicy(ctx, spec.ResolvedPolicy); err != nil {
				return err
			}
			if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
				return err
			}
			entry, inserted, err := tx.EnqueueOutbox(
				ctx, string(invocationID), KindProductionInvocationRequested, payload,
			)
			if err != nil {
				return err
			}
			if !inserted || entry.Kind != KindProductionInvocationRequested ||
				!bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf(
					"new production run did not create its ownership marker: %w",
					domain.ErrImmutableTransition,
				)
			}
		}
		return nil
	})
	if err != nil {
		return ProductionRun{}, fmt.Errorf("submit production run %q: %w", spec.RunID, err)
	}
	return ProductionRun{Run: run, InvocationID: invocationID, StageID: stageID}, nil
}

// ProductionInvocationBackupPayloadDigests validates a durable production
// dispatch intent for the local checkpoint-health scan. The request is
// self-contained and needs no external blobs.
func ProductionInvocationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeProductionRequest(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

// decodeProductionRequest reconstructs and re-checks one production dispatch
// intent against its own row. Queue payloads are opaque to the store, so the
// decoded intent is a reconstruction boundary (the same discipline as
// signet's decodeBoundInvocationRequest).
func decodeProductionRequest(entry store.QueueEntry) (productionInvocationRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(entry.Payload))
	decoder.DisallowUnknownFields()
	var request productionInvocationRequest
	if err := decoder.Decode(&request); err != nil {
		return productionInvocationRequest{}, fmt.Errorf("decode payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return productionInvocationRequest{}, errors.New("decode payload: trailing JSON value")
	}
	if entry.Kind != KindProductionInvocationRequested {
		return productionInvocationRequest{}, fmt.Errorf(
			"intent %q has kind %q: %w", entry.IdempotencyKey, entry.Kind, domain.ErrParentKeyMismatch)
	}
	if request.InvocationID == "" || request.RunID == "" || request.StageID == "" {
		return productionInvocationRequest{}, fmt.Errorf("decode payload: required identity is empty: %w", domain.ErrEmptyID)
	}
	if string(request.InvocationID) != entry.IdempotencyKey {
		return productionInvocationRequest{}, fmt.Errorf(
			"payload invocation_id %q disagrees with key %q: %w",
			request.InvocationID, entry.IdempotencyKey, domain.ErrParentKeyMismatch)
	}
	if request.InvocationID != productionInvocationID(request.RunID) ||
		request.StageID != productionStageID(request.RunID) {
		return productionInvocationRequest{}, fmt.Errorf(
			"payload identities disagree with lane derivation for run %q: %w",
			request.RunID, domain.ErrParentKeyMismatch)
	}
	return request, nil
}

// ownsProductionRun reports whether this run belongs to the production lane:
// its exact derived dispatch intent exists. The payload is reconstruction
// authority too: trusting only the kind would let a malformed or retargeted
// row classify a foreign run as production-owned before dispatch's stricter
// decoder sees it.
func (e *Engine) ownsProductionRun(ctx context.Context, run domain.Run) (bool, error) {
	var entry store.QueueEntry
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(productionInvocationID(run.ID)))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find production marker for run %q: %w", run.ID, err)
	}
	request, err := decodeProductionRequest(entry)
	if err != nil {
		return false, fmt.Errorf("authenticate production marker for run %q: %w", run.ID, err)
	}
	if request.RunID != run.ID {
		return false, fmt.Errorf(
			"production marker names run %q, loaded under %q: %w",
			request.RunID, run.ID, domain.ErrParentKeyMismatch,
		)
	}
	return true, nil
}

// loadProductionBinding resolves the durable state behind one production
// invocation. The conversation-lane fields of invocationBinding stay zero;
// stageInputSnapshot and admitAttempt already branch on the invocation's nil
// ConversationID.
func (e *Engine) loadProductionBinding(ctx context.Context, request productionInvocationRequest) (invocationBinding, error) {
	var binding invocationBinding
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding.invocation, err = tx.GetAgentInvocation(ctx, request.InvocationID)
		if err != nil {
			return err
		}
		if binding.invocation.ConversationID != nil {
			return fmt.Errorf("production invocation %q binds conversation %q: %w",
				request.InvocationID, *binding.invocation.ConversationID, domain.ErrParentKeyMismatch)
		}
		if len(binding.invocation.InputIDs) != 1 {
			return fmt.Errorf(
				"production invocation %q has %d input artifacts, want its one specification: %w",
				request.InvocationID, len(binding.invocation.InputIDs),
				domain.ErrParentKeyMismatch,
			)
		}
		specification, err := tx.GetArtifact(ctx, binding.invocation.InputIDs[0])
		if err != nil {
			return err
		}
		binding.run, err = tx.GetRun(ctx, request.RunID)
		if err != nil {
			return err
		}
		if specification.Type != productionSpecificationArtifactType ||
			specification.Digest != binding.run.SpecDigest {
			return fmt.Errorf(
				"production invocation %q input %q is not run %q's specification: %w",
				request.InvocationID, specification.ID, binding.run.ID,
				domain.ErrParentKeyMismatch,
			)
		}
		return nil
	})
	if err != nil {
		return invocationBinding{}, err
	}
	if _, ok := findProductionStage(binding.run); !ok {
		return invocationBinding{}, fmt.Errorf("run %q has no %q stage: %w",
			binding.run.ID, productionStageName, domain.ErrParentKeyMismatch)
	}
	return binding, nil
}

// productionTerminalRecord is the inbox payload recording one production
// stage's terminal outcome at most once. Status is the recorded terminal
// class: completed, failed, canceled, or gone for a session lost without a
// committed result.
type productionTerminalRecord struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        domain.RunID        `json:"run_id"`
	StageID      domain.StageID      `json:"stage_id"`
	Status       exec.Status         `json:"status"`
	HeadSHA      string              `json:"head_sha,omitempty"`
	Artifacts    []domain.Digest     `json:"artifacts,omitempty"`
	Summary      string              `json:"summary,omitempty"`
}

// decodeProductionTerminal reconstructs and re-gates one stored terminal
// record against the row it came from and the run it claims. It mirrors
// decodeProductionRequest: the same strict decode, the same derivation
// checks, so a row that survives is one this lane could itself have written.
func decodeProductionTerminal(
	entry store.QueueEntry, run domain.Run,
) (productionTerminalRecord, error) {
	if entry.Kind != kindProductionStageTerminal {
		return productionTerminalRecord{}, fmt.Errorf("inbox row %q has kind %q: %w",
			entry.IdempotencyKey, entry.Kind, domain.ErrParentKeyMismatch)
	}
	decoder := json.NewDecoder(bytes.NewReader(entry.Payload))
	decoder.DisallowUnknownFields()
	var terminal productionTerminalRecord
	if err := decoder.Decode(&terminal); err != nil {
		return productionTerminalRecord{}, fmt.Errorf("decode terminal record %q: %w",
			entry.IdempotencyKey, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return productionTerminalRecord{}, fmt.Errorf(
			"decode terminal record %q: trailing JSON value", entry.IdempotencyKey)
	}
	switch {
	case string(terminal.InvocationID) != entry.IdempotencyKey:
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q names invocation %q: %w",
			entry.IdempotencyKey, terminal.InvocationID, domain.ErrParentKeyMismatch)
	case terminal.RunID != run.ID:
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q names run %q, attempt is on %q: %w",
			entry.IdempotencyKey, terminal.RunID, run.ID, domain.ErrParentKeyMismatch)
	case terminal.InvocationID != productionInvocationID(run.ID) ||
		terminal.StageID != productionStageID(run.ID):
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q disagrees with lane derivation for run %q: %w",
			entry.IdempotencyKey, run.ID, domain.ErrParentKeyMismatch)
	// A non-terminal or unknown status is not a recorded outcome, so it must
	// not stand in for one. StatusGone is this lane's own record of a session
	// lost without a result, so it is admitted alongside the terminal set.
	case terminal.Status != exec.StatusGone && !terminal.Status.Terminal():
		return productionTerminalRecord{}, fmt.Errorf(
			"terminal record %q carries status %q: %w",
			entry.IdempotencyKey, terminal.Status, exec.ErrInvalidStatus)
	}
	return terminal, nil
}

// acceptProductionAttempt closes one production attempt: a completed result
// is re-gated before its first acceptance, while a failed, canceled, or lost
// one is recorded and surfaced as an execution_failure item instead of
// failing the reconcile loop (unattended operation must not wedge the engine
// on one bad run). The durable inbox row is the at-most-once guard, mirroring
// signet's completion intake. It reports true only for an accepted completed
// result.
func (e *Engine) acceptProductionAttempt(ctx context.Context, run domain.Run, attempt domain.Attempt) (bool, error) {
	if attempt.ID != attemptIDFor(attempt.InvocationID) {
		return false, fmt.Errorf("attempt %q disagrees with invocation %q: %w",
			attempt.ID, attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	if attempt.StageID != productionStageID(run.ID) ||
		attempt.InvocationID != productionInvocationID(run.ID) {
		return false, fmt.Errorf("attempt binding disagrees with run: %w", domain.ErrParentKeyMismatch)
	}

	var recorded *productionTerminalRecord
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, string(attempt.InvocationID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// A stored row is a reconstruction boundary, not authority. Trusting
		// the kind alone would let a corrupted or fabricated row permanently
		// suppress this attempt's collection: no accepted result, and no
		// execution_failure item either, which is the one outcome that makes
		// a failure invisible rather than loud.
		terminal, err := decodeProductionTerminal(entry, run)
		if err != nil {
			return err
		}
		recorded = &terminal
		return nil
	})
	if err != nil {
		return false, err
	}
	if recorded != nil {
		if err := e.authenticateProductionTerminal(ctx, run, attempt, *recorded); err != nil {
			return false, err
		}
		return false, nil
	}

	result, ready, err := e.collectTerminal(ctx, attempt)
	lost := false
	switch {
	case errors.Is(err, ErrInvocationLost):
		lost = true
	case MutableAdmissionPolicyRefusal(err):
		return false, nil
	case err != nil:
		return false, err
	case !ready:
		return false, nil
	}

	terminal := productionTerminalRecord{
		InvocationID: attempt.InvocationID, RunID: run.ID, StageID: attempt.StageID,
		Status: exec.StatusGone,
	}
	if !lost {
		terminal.Status = result.Status
		terminal.HeadSHA = result.HeadSHA
		terminal.Artifacts = result.Artifacts
		terminal.Summary = result.Summary
	}
	if terminal.Status == exec.StatusCompleted {
		// Re-gated at the last point the engine controls before the
		// acceptance commits, for the same reason acceptAttempt re-gates: the
		// trust anchor of an unattended or waived admission can move while a
		// driver call is in flight.
		if err := e.requireProductionAdmissible(ctx, attempt.InvocationID); err != nil {
			if MutableAdmissionPolicyRefusal(err) {
				return false, nil
			}
			return false, err
		}
	}
	accepted, err := e.recordProductionTerminal(ctx, run, terminal)
	if MutableAdmissionPolicyRefusal(err) {
		return false, nil
	}
	return accepted, err
}

func (e *Engine) authenticateProductionTerminal(
	ctx context.Context, run domain.Run, attempt domain.Attempt, recorded productionTerminalRecord,
) error {
	if err := e.requireProductionAdmissionRecord(ctx, attempt.InvocationID); err != nil {
		return err
	}
	result, ready, err := e.collectTerminal(ctx, attempt)
	lost := errors.Is(err, ErrInvocationLost)
	if err != nil && !lost {
		return err
	}
	if !lost && !ready {
		return fmt.Errorf("terminal record for %q has no collectable driver outcome: %w",
			attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	actual := productionTerminalRecord{
		InvocationID: attempt.InvocationID, RunID: run.ID, StageID: attempt.StageID,
		Status: exec.StatusGone,
	}
	if !lost {
		actual.Status = result.Status
		actual.HeadSHA = result.HeadSHA
		actual.Artifacts = result.Artifacts
		actual.Summary = result.Summary
	}
	if recorded.InvocationID != actual.InvocationID ||
		recorded.RunID != actual.RunID ||
		recorded.StageID != actual.StageID ||
		recorded.Status != actual.Status ||
		recorded.HeadSHA != actual.HeadSHA ||
		!slices.Equal(recorded.Artifacts, actual.Artifacts) ||
		recorded.Summary != actual.Summary {
		return fmt.Errorf("terminal record for %q disagrees with collected outcome: %w",
			attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	return nil
}

// requireProductionAdmissionRecord authenticates the immutable admission
// authority for terminal history without re-applying mutable current policy.
// A recorded terminal has already crossed the current-policy acceptance gate;
// replay only proves that the durable record still agrees with the driver.
func (e *Engine) requireProductionAdmissionRecord(
	ctx context.Context, invocationID domain.InvocationID,
) error {
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
		return err
	})
	if err != nil {
		return fmt.Errorf("production invocation %q has no authentic admission record: %w",
			invocationID, err)
	}
	return nil
}

// requireProductionAdmissible applies the same current-policy reconstruction
// gate as the legacy lane, but also requires the admission row to exist.
// Production dispatch cannot create an attempt without that row, so absence
// later is lost audit authority, never a pre-admission legacy attempt.
func (e *Engine) requireProductionAdmissible(
	ctx context.Context, invocationID domain.InvocationID,
) error {
	found := false
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		_, foundNow, err := tx.LookupExecutionAdmission(ctx, invocationID)
		found = foundNow
		return err
	})
	if err != nil {
		return fmt.Errorf("production invocation %q is no longer admissible: %w",
			invocationID, err)
	}
	if !found {
		return fmt.Errorf("production invocation %q has no durable admission: %w",
			invocationID, store.ErrNotFound)
	}
	return nil
}

// recordProductionTerminal commits one terminal record and, for anything but
// a completed result, the deterministic execution_failure item in the same
// transaction. A converged replay (row already present with identical bytes)
// writes nothing.
func (e *Engine) recordProductionTerminal(
	ctx context.Context, run domain.Run, terminal productionTerminalRecord,
) (bool, error) {
	payload, err := json.Marshal(terminal)
	if err != nil {
		return false, fmt.Errorf("record terminal for %q: %w", terminal.InvocationID, err)
	}
	inserted := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		if terminal.Status == exec.StatusCompleted {
			// Repeat the current-policy gate in the terminal write
			// transaction. The earlier check closes the driver-call window;
			// this one prevents a trust-profile update from interleaving
			// between that check and acceptance.
			if _, err := tx.GetExecutionAdmission(ctx, terminal.InvocationID); err != nil {
				return err
			}
		}
		entry, insertedNow, err := tx.RecordInbox(
			ctx, string(terminal.InvocationID), kindProductionStageTerminal, payload)
		if err != nil {
			return err
		}
		if !insertedNow {
			if entry.Kind != kindProductionStageTerminal || !bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf("terminal record for %q disagrees with stored row: %w",
					terminal.InvocationID, domain.ErrImmutableTransition)
			}
			return errReplay
		}
		inserted = true
		if terminal.Status != exec.StatusCompleted {
			item, err := productionFailureItem(run, terminal)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record terminal for %q: %w", terminal.InvocationID, err)
	}
	return inserted && terminal.Status == exec.StatusCompleted, nil
}

// productionFailureItem is the §4 execution_failure notice for a production
// stage that ended without an accepted result. Deterministic identity and
// content, so a replayed pass converges instead of raising a second item.
// Only acknowledge is offered: retry has no honoring machinery yet, and an
// action the system cannot honour is worse than an absent one (the
// waived-posture precedent).
func productionFailureItem(run domain.Run, terminal productionTerminalRecord) (domain.AttentionItem, error) {
	runID := run.ID
	reason := fmt.Sprintf("Unattended %s stage ended %q without an accepted result.",
		productionStageName, terminal.Status)
	if terminal.Summary != "" {
		reason += " Driver summary: " + terminal.Summary
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID("execution-failure-" + string(terminal.InvocationID)),
		ProjectID: run.ProjectID,
		Subject:   domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:      domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason:            reason,
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
}

func findProductionStage(run domain.Run) (domain.Stage, bool) {
	for _, stage := range run.Stages {
		if stage.ID == productionStageID(run.ID) && stage.Name == productionStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}
