package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// This value mirrors signet's private outbox kind. The string is durable
// storage vocabulary, not an exported signet API: the engine is the intended
// production consumer while signet remains the producer.
const kindAgentInvocationRequested = string(domain.AgentInvocationRequestedKind)

type invocationRequest struct {
	InvocationID   domain.InvocationID   `json:"invocation_id"`
	ConversationID domain.ConversationID `json:"conversation_id"`
	ItemID         domain.ItemID         `json:"item_id"`
	ItemVersion    int                   `json:"item_version"`
}

type invocationBinding struct {
	run          domain.Run
	item         domain.AttentionItem
	invocation   domain.AgentInvocation
	conversation domain.Conversation
}

func (e *Engine) reconcileInvocations(ctx context.Context) (int, int, error) {
	started, err := e.dispatchPendingInvocations(ctx)
	if err != nil {
		return started, 0, err
	}
	accepted, err := e.acceptCompletedInvocations(ctx)
	if err != nil {
		return started, accepted, err
	}
	return started, accepted, nil
}

// dispatchPendingInvocations converts signet's committed outbox request into
// durable engine state before starting the external effect. The Run attempt is
// the engine's restart index; after Start succeeds, the outbox row can follow
// its store contract and become dispatched without making the result
// undiscoverable after a daemon restart.
func (e *Engine) dispatchPendingInvocations(ctx context.Context) (int, error) {
	if e.refreshUnattendedHealth != nil {
		if err := e.refreshUnattendedHealth(ctx); err != nil {
			return 0, fmt.Errorf("refresh unattended health before dispatch: %w", err)
		}
	}
	var (
		pending            []store.QueueEntry
		pendingProduction  []store.QueueEntry
		pendingElaboration []store.QueueEntry
		pendingDiscussion  []store.QueueEntry
		pendingRemediation []store.QueueEntry
		held               bool
		holdReason         domain.RunHoldReason
	)
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		// An engine that is not explicitly configured attended_dev honours
		// the whole operating-state gate — the durable operator stop and
		// the blocking system_health rule — before dispatching anything
		// (#319, #321): the intents stay pending, the pass reports no error
		// (a hold is operator-visible state in signet, not a failure, and a
		// Run-loop error would halt attended work too), and a resume or a
		// cleared blocker makes a later pass pick them up. An engine with
		// no admission configuration is included: its dispatches record no
		// operating mode, so whether they are unattended is unknowable, and
		// unknown fails closed against the full gate — the one shared
		// predicate, so this path cannot cover one half and miss the other
		// (a daemon restarted without its configuration runs no
		// in-transaction gate at all for an unrecorded intent). Every
		// durable window routes through this per-pass check; the residual
		// in-process race with a hold committing mid-pass is closed for
		// admitted work by the store's gate inside each admitting
		// transaction, mapped to the same quiet hold below.
		if e.admission == nil || e.admission.environment.OperatingMode == domain.ModeUnattended {
			if err := tx.RequireUnattendedOperationOpen(ctx); err != nil {
				// A held pass still lists the pending production intents so
				// the hold and its typed cause are observable per run
				// (issue #394); the listing is a read, and dispatch below is
				// skipped exactly as before.
				switch {
				case errors.Is(err, domain.ErrUnattendedOperationStopped):
					held, holdReason = true, domain.HoldOperationStopped
				case errors.Is(err, domain.ErrBlockingSystemHealth):
					held, holdReason = true, domain.HoldBlockingSystemHealth
				default:
					return err
				}
			}
		}
		var err error
		pending, err = tx.ListPendingOutbox(ctx, kindAgentInvocationRequested)
		if err != nil {
			return err
		}
		pendingProduction, err = tx.ListPendingOutbox(ctx, KindProductionInvocationRequested)
		if err != nil {
			return err
		}
		pendingElaboration, err = tx.ListPendingOutbox(ctx, KindElaborationInvocationRequested)
		if err != nil {
			return err
		}
		pendingDiscussion, err = tx.ListPendingOutbox(ctx, KindElaborationDiscussionRequested)
		if err != nil {
			return err
		}
		pendingRemediation, err = tx.ListPendingOutbox(ctx, KindRemediationInvocationRequested)
		return err
	})
	if err != nil {
		return 0, err
	}
	if held {
		for _, entry := range pendingProduction {
			invocationID := domain.InvocationID(entry.IdempotencyKey)
			runID, attributable := productionRunIDFromInvocationID(invocationID)
			if !attributable {
				continue
			}
			if err := e.observeRunHold(ctx, runID, invocationID, holdReason); err != nil {
				return 0, err
			}
		}
		for _, entry := range pendingElaboration {
			request, binding, err := e.loadElaborationBinding(ctx, entry)
			if err != nil {
				quarantined, quarantineErr := e.quarantinePendingElaborationMarker(ctx, entry, err)
				if quarantineErr != nil {
					return 0, quarantineErr
				}
				if quarantined {
					continue
				}
				return 0, err
			}
			if err := e.observeRunHold(ctx, binding.run.ID, request.InvocationID, holdReason); err != nil {
				return 0, err
			}
		}
		for _, entry := range pendingDiscussion {
			request, binding, err := e.loadElaborationDiscussionBinding(ctx, entry)
			if err != nil {
				quarantined, quarantineErr := e.quarantinePendingElaborationDiscussionMarker(ctx, entry, err)
				if quarantineErr != nil {
					return 0, quarantineErr
				}
				if quarantined {
					continue
				}
				return 0, err
			}
			if err := e.observeRunHold(ctx, binding.run.ID, request.InvocationID, holdReason); err != nil {
				return 0, err
			}
		}
		for _, entry := range pendingRemediation {
			request, err := decodeRemediationRequest(entry)
			if err != nil {
				quarantined, quarantineErr := e.quarantinePendingRemediationMarker(ctx, entry, err)
				if quarantineErr != nil {
					return 0, quarantineErr
				}
				if quarantined {
					continue
				}
				return 0, err
			}
			binding, err := e.loadRemediationBinding(ctx, request)
			if err != nil {
				quarantined, quarantineErr := e.quarantinePendingRemediationMarker(ctx, entry, err)
				if quarantineErr != nil {
					return 0, quarantineErr
				}
				if quarantined {
					continue
				}
				return 0, err
			}
			if err := e.observeRunHold(ctx, binding.run.ID, request.InvocationID, holdReason); err != nil {
				return 0, err
			}
		}
		return 0, nil
	}

	started := 0
	for _, entry := range pending {
		request, binding, err := e.loadInvocationRequest(ctx, entry)
		if err != nil {
			if errors.Is(err, errForeignWorkflow) {
				continue
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		stage, ok := findFeedbackStage(binding.run)
		if !ok {
			return started, fmt.Errorf("intent %q: run %q has no feedback stage",
				entry.IdempotencyKey, binding.run.ID)
		}
		startedNow, hold, err := e.dispatchIntent(ctx, entry, binding, stage, request.InvocationID)
		started += boolCount(startedNow)
		if err != nil {
			if reason, ok := dispatchHoldReason(err); ok {
				if obsErr := e.observeRunHold(ctx, binding.run.ID, request.InvocationID, reason); obsErr != nil {
					return started, obsErr
				}
			}
			if invocationDispatchHold(err) {
				continue
			}
			if unattendedDispatchRefusal(err) {
				return started, nil
			}
			return started, err
		}
		if hold {
			return started, nil
		}
	}
	for _, entry := range pendingElaboration {
		if e.elaboration == nil {
			continue
		}
		request, binding, err := e.loadElaborationBinding(ctx, entry)
		if err != nil {
			quarantined, quarantineErr := e.quarantinePendingElaborationMarker(ctx, entry, err)
			if quarantineErr != nil {
				return started, quarantineErr
			}
			if quarantined {
				continue
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		if err := releaseProductionQuarantine(
			ctx, e.store, e.signet, elaborationMarkerQuarantinePrefix, binding.run.ID,
		); err != nil {
			return started, err
		}
		// Submission records durable intent before a daemon composition is
		// available to materialize it. Classify a permanent source-input
		// refusal here, before recording an attempt, so a valid submission
		// cannot remain pending forever when the elaborator cannot carry it.
		if !attemptRecorded(binding.run, request.InvocationID) {
			if err := e.validateElaborationInvocationDelivery(ctx, binding.run, binding.invocation); err != nil {
				if errors.Is(err, ErrElaborationInputUndeliverable) {
					if err := e.recordElaborationFailure(ctx, binding.run, request, exec.StatusFailed, err.Error()); err != nil {
						return started, err
					}
					continue
				}
				return started, err
			}
		}
		if attemptRecorded(binding.run, request.InvocationID) {
			var admission domain.ExecutionAdmission
			if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				admission, err = tx.GetExecutionAdmissionRecord(ctx, request.InvocationID)
				return err
			}); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					cause := fmt.Errorf("%w: elaboration admission is missing", errElaborationMarkerUnreadable)
					quarantined, quarantineErr := e.quarantinePendingElaborationMarker(ctx, entry, cause)
					if quarantineErr != nil {
						return started, quarantineErr
					}
					if quarantined {
						continue
					}
				}
				return started, fmt.Errorf("intent %q admission: %w", entry.IdempotencyKey, err)
			}
			if admission.EgressProfile != domain.EgressProviderOnly {
				cause := fmt.Errorf("%w: elaboration admission has egress %q",
					errElaborationMarkerUnreadable, admission.EgressProfile)
				quarantined, quarantineErr := e.quarantinePendingElaborationMarker(ctx, entry, cause)
				if quarantineErr != nil {
					return started, quarantineErr
				}
				if quarantined {
					continue
				}
				return started, cause
			}
		} else if e.admission == nil || e.admission.environment.EgressProfile != domain.EgressProviderOnly {
			return started, fmt.Errorf("elaboration requires provider_only admission: %w", exec.ErrCapabilityRefused)
		}
		stage, ok := findElaborationStage(binding.run)
		if !ok {
			cause := fmt.Errorf("%w: elaboration stage missing", errElaborationMarkerUnreadable)
			quarantined, quarantineErr := e.quarantinePendingElaborationMarker(ctx, entry, cause)
			if quarantineErr != nil {
				return started, quarantineErr
			}
			if quarantined {
				continue
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, cause)
		}
		startedNow, hold, err := e.dispatchIntent(ctx, entry, binding, stage, request.InvocationID)
		started += boolCount(startedNow)
		if err != nil {
			if reason, ok := dispatchHoldReason(err); ok {
				if obsErr := e.observeRunHold(ctx, binding.run.ID, request.InvocationID, reason); obsErr != nil {
					return started, obsErr
				}
			}
			if invocationDispatchHold(err) {
				continue
			}
			if unattendedDispatchRefusal(err) {
				return started, nil
			}
			return started, err
		}
		if hold {
			return started, nil
		}
	}
	for _, entry := range pendingDiscussion {
		if e.elaboration == nil {
			continue
		}
		request, binding, err := e.loadElaborationDiscussionBinding(ctx, entry)
		if err != nil {
			quarantined, quarantineErr := e.quarantinePendingElaborationDiscussionMarker(ctx, entry, err)
			if quarantineErr != nil {
				return started, quarantineErr
			}
			if quarantined {
				continue
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		if err := releaseProductionQuarantine(
			ctx, e.store, e.signet, elaborationDiscussionMarkerQuarantinePrefix, binding.run.ID,
		); err != nil {
			return started, err
		}
		if attemptRecorded(binding.run, request.InvocationID) {
			var admission domain.ExecutionAdmission
			if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				admission, err = tx.GetExecutionAdmissionRecord(ctx, request.InvocationID)
				return err
			}); err != nil {
				return started, fmt.Errorf("intent %q admission: %w", entry.IdempotencyKey, err)
			}
			if admission.EgressProfile != domain.EgressProviderOnly {
				return started, fmt.Errorf("discussion admission has egress %q: %w",
					admission.EgressProfile, exec.ErrCapabilityRefused)
			}
		} else {
			deliveryErr := e.validateProspectiveDelivery(
				ctx, binding.run, binding.invocation, e.elaboration.promptPackage, true, nil,
			)
			if errors.Is(deliveryErr, ErrElaborationInputUndeliverable) {
				if err := e.acceptSpecDiscussionReply(ctx, request, unavailableSpecDiscussionReply); err != nil {
					return started, err
				}
				continue
			}
			if deliveryErr != nil {
				return started, deliveryErr
			}
			if e.admission == nil || e.admission.environment.EgressProfile != domain.EgressProviderOnly {
				return started, fmt.Errorf("elaboration discussion requires provider_only admission: %w", exec.ErrCapabilityRefused)
			}
		}
		stage, ok := findElaborationStage(binding.run)
		if !ok {
			return started, fmt.Errorf("intent %q: elaboration stage missing", entry.IdempotencyKey)
		}
		startedNow, hold, err := e.dispatchIntent(ctx, entry, binding, stage, request.InvocationID)
		started += boolCount(startedNow)
		if err != nil {
			if reason, ok := dispatchHoldReason(err); ok {
				if obsErr := e.observeRunHold(ctx, binding.run.ID, request.InvocationID, reason); obsErr != nil {
					return started, obsErr
				}
			}
			if invocationDispatchHold(err) {
				continue
			}
			if unattendedDispatchRefusal(err) {
				return started, nil
			}
			return started, err
		}
		if hold {
			return started, nil
		}
	}
	for _, entry := range pendingRemediation {
		request, err := decodeRemediationRequest(entry)
		if err != nil {
			quarantined, quarantineErr := e.quarantinePendingRemediationMarker(ctx, entry, err)
			if quarantineErr != nil {
				return started, quarantineErr
			}
			if quarantined {
				continue
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		binding, err := e.loadRemediationBinding(ctx, request)
		if err != nil {
			quarantined, quarantineErr := e.quarantinePendingRemediationMarker(ctx, entry, err)
			if quarantineErr != nil {
				return started, quarantineErr
			}
			if quarantined {
				continue
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		if err := releaseProductionQuarantine(
			ctx, e.store, e.signet, remediationMarkerQuarantinePrefix, binding.run.ID,
		); err != nil {
			return started, err
		}
		stage, ok := findRemediationStage(binding.run, request.Round)
		if !ok {
			return started, fmt.Errorf("intent %q: run %q has no remediation stage",
				entry.IdempotencyKey, binding.run.ID)
		}
		startedNow, hold, err := e.dispatchIntent(ctx, entry, binding, stage, request.InvocationID)
		started += boolCount(startedNow)
		if err != nil {
			if errors.Is(err, ErrRemediationInputUndeliverable) {
				if failureErr := e.recordProductionDeliveryRefusal(
					ctx, binding.run, stage, request.InvocationID, err.Error(),
				); failureErr != nil {
					return started, failureErr
				}
				continue
			}
			if reason, ok := dispatchHoldReason(err); ok {
				if obsErr := e.observeRunHold(ctx, binding.run.ID, request.InvocationID, reason); obsErr != nil {
					return started, obsErr
				}
			}
			if invocationDispatchHold(err) {
				continue
			}
			if unattendedDispatchRefusal(err) {
				return started, nil
			}
			return started, err
		}
		if hold {
			return started, nil
		}
	}
	for _, entry := range pendingProduction {
		// The production kind is engine-owned, so a malformed row is broken
		// owned state, never another workflow's: it is quarantined behind an
		// operator notice and skipped, never dispatched. Failing the pass
		// instead would hold every later healthy intent in the ordered outbox
		// behind one row that no pass can ever decode (#424). A row this lane
		// could not have filed names no run to quarantine, so it stays loud.
		markerRunID, attributable := productionRunIDFromInvocationID(
			domain.InvocationID(entry.IdempotencyKey))
		request, err := authenticateProductionMarker(entry, markerRunID)
		if err != nil {
			if attributable {
				quarantined, quarantineErr := e.quarantinePendingProductionMarker(
					ctx, markerRunID, err)
				if quarantineErr != nil {
					return started, quarantineErr
				}
				if quarantined {
					continue
				}
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		// The production lane is real unattended execution. A missing or
		// attended admission composition keeps the intent durable and pending;
		// starting it would either hand the driver a zero spec or violate
		// attended_dev's prohibition on automatic publication. Hold rather than
		// fail so the daemon's other lanes remain healthy and an unattended
		// composition can pick up the same row untouched.
		if e.admission == nil || e.admission.environment.OperatingMode != domain.ModeUnattended {
			// Every queued production run is held by this composition, not
			// just the entry that surfaced the condition, and the ordered
			// outbox would stop each later pass on this same oldest row: so
			// record the typed hold for this authenticated entry and every
			// remaining attributable one (projection writes only; the
			// remaining rows keep their ordinary authentication for the
			// unattended composition that eventually dispatches them), then
			// return exactly as before.
			if err := e.observeRunHold(ctx, request.RunID, request.InvocationID,
				domain.HoldAttendedModeActive); err != nil {
				return started, err
			}
			for _, held := range pendingProduction {
				if held.IdempotencyKey == entry.IdempotencyKey {
					continue
				}
				invocationID := domain.InvocationID(held.IdempotencyKey)
				runID, attributable := productionRunIDFromInvocationID(invocationID)
				if !attributable {
					continue
				}
				if err := e.observeRunHold(ctx, runID, invocationID,
					domain.HoldAttendedModeActive); err != nil {
					return started, err
				}
			}
			return started, nil
		}
		binding, err := e.loadProductionBinding(ctx, request)
		if err != nil {
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		stage, ok := findProductionStage(binding.run)
		if !ok {
			return started, fmt.Errorf("intent %q: run %q has no %s stage",
				entry.IdempotencyKey, binding.run.ID, productionStageName)
		}
		startedNow, hold, err := e.dispatchIntent(ctx, entry, binding, stage, request.InvocationID)
		started += boolCount(startedNow)
		if err != nil {
			if errors.Is(err, ErrProductionInputUndeliverable) {
				if failureErr := e.recordProductionDeliveryRefusal(
					ctx, binding.run, stage, request.InvocationID, err.Error(),
				); failureErr != nil {
					return started, failureErr
				}
				continue
			}
			// A classified hold or refusal is operator-visible run state:
			// record its typed cause before taking the same quiet path as
			// before (issue #394).
			if reason, ok := dispatchHoldReason(err); ok &&
				(invocationDispatchHold(err) || unattendedDispatchRefusal(err)) {
				if obsErr := e.observeRunHold(ctx, request.RunID, request.InvocationID, reason); obsErr != nil {
					return started, obsErr
				}
			}
			// Input materialization is scoped to this invocation. Preserve its
			// pending row, but continue so one unavailable blob cannot starve
			// every later healthy submission in the ordered outbox.
			if invocationDispatchHold(err) {
				continue
			}
			// A backend below the floor holds unattended work instead of
			// ending the daemon's reconcile loop. The refusal is already a
			// typed no-op — nothing appended, nothing started — and its own
			// contract is that the intent stays pending for a pass under a
			// conformant backend. Exiting here would mean a daemon started
			// before its conformance record was produced could never pick the
			// work up, which is the opposite of running unattended.
			if unattendedDispatchRefusal(err) {
				return started, nil
			}
			return started, err
		}
		if hold {
			return started, nil
		}
	}
	return started, nil
}

func unattendedDispatchRefusal(err error) bool {
	return errors.Is(err, exec.ErrCapabilityRefused) ||
		errors.Is(err, exec.ErrPreJobRefused) ||
		MutableAdmissionPolicyRefusal(err)
}

func invocationDispatchHold(err error) bool {
	return errors.Is(err, exec.ErrInputUnavailable) ||
		errors.Is(err, domain.ErrIdentityParallelismExhausted)
}

// MutableAdmissionPolicyRefusal identifies a fail-closed current-policy
// verdict that can change without changing the recorded attempt. These
// verdicts hold work for a later reconcile pass. Immutable-record corruption,
// identity inconsistencies, and binding errors are deliberately absent: those
// remain fatal correctness failures.
func MutableAdmissionPolicyRefusal(err error) bool {
	return backendConformanceRefusal(err) ||
		errors.Is(err, domain.ErrUnknownAdmissionFloor) ||
		errors.Is(err, domain.ErrCapabilityBelowFloor) ||
		errors.Is(err, domain.ErrCredentialModeNotApproved) ||
		errors.Is(err, domain.ErrWaiverNotConfigured) ||
		errors.Is(err, domain.ErrBackupHealthUnavailable) ||
		errors.Is(err, domain.ErrCheckpointNotEncrypted) ||
		errors.Is(err, domain.ErrCheckpointNotCurrent) ||
		errors.Is(err, domain.ErrArtifactClosureIncomplete) ||
		errors.Is(err, domain.ErrRestoreTestStale) ||
		errors.Is(err, domain.ErrInvalidBackupHealthStatus) ||
		errors.Is(err, store.ErrRepositoryUntrusted) ||
		errors.Is(err, publish.ErrJanitorInactive) ||
		errors.Is(err, domain.ErrRepositoryIdentityMismatch) ||
		errors.Is(err, domain.ErrPathBoundaryMismatch) ||
		errors.Is(err, domain.ErrTrustProfileSuperseded) ||
		errors.Is(err, domain.ErrReviewConfigurationUnapproved)
}

func backendConformanceRefusal(err error) bool {
	return errors.Is(err, store.ErrBackendNotConformant) ||
		errors.Is(err, domain.ErrConformanceConfigurationUnbound) ||
		errors.Is(err, domain.ErrAdmissionConfigurationMismatch) ||
		errors.Is(err, domain.ErrAdmissionExceedsConformance)
}

// dispatchIntent runs the lane-independent half of one dispatch: admit if no
// attempt exists, record attempt and admission durably, start the driver, and
// mark the intent dispatched. hold reports the operating-state refusal that
// should quietly end the pass (see the callers' pre-check).
func (e *Engine) dispatchIntent(
	ctx context.Context, entry store.QueueEntry, binding invocationBinding,
	stage domain.Stage, invocationID domain.InvocationID,
) (bool, bool, error) {
	// Durable state decides first. The run snapshot already says whether
	// this invocation has an attempt, and a recorded attempt starts under
	// the admission stored beside it, so the live capability gate runs
	// only for an attempt that does not exist yet. Admitting first would
	// let a backend that no longer clears the floor strand work that was
	// already admitted, which is the same mistake as letting the current
	// configuration decide whether to read the record at all.
	var fresh *domain.ExecutionAdmission
	if !attemptRecorded(binding.run, invocationID) {
		admission, admitted, err := e.admitAttempt(ctx, binding, stage, invocationID)
		if err != nil {
			// A backend below the floor is a typed refusal (§5.7): nothing
			// is appended and nothing is started, so the intent stays
			// pending for a pass under a backend that clears the floor.
			return false, false, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		if admitted {
			fresh = &admission
		}
	}
	// recordAttempt reports the admission that is actually durable: on a
	// replay it is the stored one, not a freshly built value whose
	// admission instant (and therefore identity) has moved since. Starting
	// under a fresh id would hand the driver an admission no reader can
	// reconstruct.
	_, effective, bound, err := e.recordAttempt(
		ctx, binding.run.ID, stage.ID, invocationID, entry.Status, fresh,
	)
	if err != nil {
		// The admitting transaction's operating-state refusal (a stop or a
		// blocking system_health item committed since the pass began) is
		// the pre-check's race-free backstop: hold the remaining intents
		// for a later pass instead of failing the loop. The operator
		// already sees why — the stop notice or the blocking item is open
		// in signet.
		if errors.Is(err, domain.ErrUnattendedOperationStopped) ||
			errors.Is(err, domain.ErrBlockingSystemHealth) {
			if entry.Kind == KindProductionInvocationRequested {
				if reason, ok := dispatchHoldReason(err); ok {
					if obsErr := e.observeRunHold(ctx, binding.run.ID, invocationID, reason); obsErr != nil {
						return false, false, obsErr
					}
				}
			}
			return false, true, nil
		}
		return false, false, err
	}
	// A pre-fix daemon may already have recorded a production admission that
	// exceeds the configured driver's deterministic prompt boundary. Inspect
	// first: Start can have succeeded before the outbox dispatch bit committed,
	// and a known invocation's eventual result outranks a replay-time delivery
	// refusal. Only a driver-proven unknown invocation is still safe to refuse.
	if bound && fresh == nil {
		if err := e.validateProductionReplayDelivery(ctx, invocationID, effective); err != nil {
			return false, false, fmt.Errorf(
				"intent %q production delivery: %w", entry.IdempotencyKey, err)
		}
	}
	startSpec := exec.StartSpec{RunID: binding.run.ID, StageID: stage.ID}
	if bound {
		startSpec = exec.StartSpecFromAdmission(effective)
	}
	startedNow := false
	if err := e.driver.Start(ctx, invocationID, startSpec); err != nil {
		if !errors.Is(err, exec.ErrDuplicateStart) {
			if bound && effective.AuthIdentityID != nil {
				if releaseErr := e.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
					return tx.ReleaseOutboxDispatch(ctx, string(invocationID))
				}); releaseErr != nil {
					return false, false, fmt.Errorf("intent %q release dispatch reservation: %w",
						entry.IdempotencyKey, errors.Join(err, releaseErr))
				}
			}
			return false, false, fmt.Errorf("intent %q: start: %w", entry.IdempotencyKey, err)
		}
	} else {
		startedNow = true
	}
	if err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey); err != nil {
			return err
		}
		// A capacity hold is current scheduling state, not history. Clear only
		// that exact cause when this invocation starts; the store's predicate
		// preserves a newer, different hold that may have replaced it.
		if err := tx.ClearRunHoldCause(
			ctx, binding.run.ID, domain.HoldIdentityParallelism,
		); err != nil {
			return err
		}
		// The started milestone rides the dispatch bookkeeping transaction:
		// it also converges for a replay whose Start already happened
		// (ErrDuplicateStart above), because the fact being recorded is that
		// the invocation was handed to the driver, not that it was handed
		// over just now (issue #394).
		invocation := invocationID
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: binding.run.ID, Kind: domain.MilestoneInvocationStarted,
			InvocationID: &invocation, RecordedAt: time.Now().UTC(),
		})
	}); err != nil {
		// The start already happened, so it counts even though the
		// bookkeeping mark failed; the caller adds startedNow before
		// returning the error.
		return startedNow, false, fmt.Errorf("intent %q: mark dispatched: %w", entry.IdempotencyKey, err)
	}
	return startedNow, false, nil
}

func (e *Engine) acceptCompletedInvocations(ctx context.Context) (int, error) {
	var runs []store.Snapshotted[domain.Run]
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		runs, err = tx.ListRuns(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}

	accepted := 0
	for _, snapshotted := range runs {
		run := snapshotted.Value
		owned, err := e.ownsFakeRun(ctx, run)
		if err != nil {
			return accepted, err
		}
		if owned {
			stage, ok := findFeedbackStage(run)
			if !ok {
				continue
			}
			for _, attempt := range stage.Attempts {
				didAccept, err := e.acceptAttempt(ctx, run, attempt)
				if err != nil {
					return accepted, fmt.Errorf("run %q invocation %q: %w", run.ID, attempt.InvocationID, err)
				}
				accepted += boolCount(didAccept)
			}
			continue
		}
		ownedProduction, err := e.ownsProductionRun(ctx, run)
		if err != nil {
			return accepted, err
		}
		if !ownedProduction {
			ownedElaboration, err := e.ownsElaborationRun(ctx, run)
			if err != nil {
				return accepted, err
			}
			if !ownedElaboration {
				continue
			}
			stage, ok := findElaborationStage(run)
			if !ok {
				continue
			}
			for _, attempt := range stage.Attempts {
				didAccept, err := e.acceptElaborationAttempt(ctx, run, attempt)
				if err != nil {
					return accepted, fmt.Errorf("run %q invocation %q: %w", run.ID, attempt.InvocationID, err)
				}
				accepted += boolCount(didAccept)
			}
			continue
		}
		for _, stage := range run.Stages {
			if len(stage.Attempts) == 0 {
				continue
			}
			expected, ok := productionStageForInvocation(run, stage.Attempts[0].InvocationID)
			if !ok || expected.ID != stage.ID {
				continue
			}
			for _, attempt := range stage.Attempts {
				didAccept, err := e.acceptProductionAttempt(ctx, run, attempt)
				if err != nil {
					return accepted, fmt.Errorf("run %q invocation %q: %w", run.ID, attempt.InvocationID, err)
				}
				accepted += boolCount(didAccept)
			}
		}
	}
	return accepted, nil
}

func (e *Engine) acceptAttempt(ctx context.Context, run domain.Run, attempt domain.Attempt) (bool, error) {
	if attempt.ID != attemptIDFor(attempt.InvocationID) {
		return false, fmt.Errorf("attempt %q disagrees with invocation %q: %w",
			attempt.ID, attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	binding, err := e.loadInvocationBinding(ctx, attempt.InvocationID)
	if err != nil {
		return false, err
	}
	if binding.run.ID != run.ID || attempt.StageID != feedbackStageID(run.ID) {
		return false, fmt.Errorf("attempt binding disagrees with run: %w", domain.ErrParentKeyMismatch)
	}
	accepted, err := completionAlreadyAccepted(binding.conversation, attempt.InvocationID)
	if err != nil {
		return false, err
	}
	if accepted {
		return false, nil
	}

	result, ready, err := e.collectTerminal(ctx, run.ID, attempt)
	if err != nil {
		return false, err
	}
	if !ready {
		return false, nil
	}
	if result.Status != exec.StatusCompleted {
		return false, fmt.Errorf("result status %q: %w", result.Status, ErrInvocationUnsuccessful)
	}

	if err := e.signet.AcceptAgentCompletion(ctx, attempt.InvocationID, signet.AgentReply{
		Body: result.Summary, Attachments: result.Artifacts,
	}, signet.WithPreCommitGate(func(ctx context.Context, tx *store.ReadTx) error {
		// Reading the admission re-runs the mutable reconstruction gate. Signet
		// invokes this closure after dedup and inside the same transaction that
		// commits a genuinely new completion, so the verdict cannot go stale
		// before acceptance.
		_, _, err := tx.LookupExecutionAdmission(ctx, attempt.InvocationID)
		if err != nil {
			return fmt.Errorf("invocation %q is no longer admissible: %w", attempt.InvocationID, err)
		}
		return nil
	})); err != nil {
		return false, fmt.Errorf("accept result: %w", err)
	}
	return true, nil
}

// collectTerminal inspects one attempt and collects its terminal result:
// the lane-independent half of acceptance. ready is false while the
// invocation is still pending or running, or while an unknown invocation's
// intent is still pending dispatch. A gone session without a committed
// result returns an error wrapping ErrInvocationLost; the lanes decide
// whether that is a loop failure (walking skeleton) or a terminal outcome
// to record (production).
func (e *Engine) collectTerminal(ctx context.Context, runID domain.RunID, attempt domain.Attempt) (exec.StageResult, bool, error) {
	inspection, err := e.driver.Inspect(ctx, attempt.InvocationID)
	if err != nil {
		// An invocation the driver does not know is ambiguous: a pending
		// outbox intent is bookkeeping, not proof of an unstarted driver
		// (Start can succeed and the daemon die before MarkOutboxDispatched).
		// The driver's own answer disambiguates: unknown *and* still pending
		// means the launch never happened — the crash window between the
		// admitting transaction and Start, or a dispatch held by the
		// operator stop (#319) — and the start is the dispatch loop's to
		// replay, so acceptance waits without error. Unknown but already
		// dispatched is a genuinely lost invocation and stays the failure it
		// always was. A known invocation proceeds to collection regardless
		// of the intent's state: a stop halts new starts, never the
		// acceptance of work already running.
		if errors.Is(err, exec.ErrUnknownInvocation) {
			var pendingDispatch bool
			if outboxErr := e.store.Read(ctx, func(tx *store.ReadTx) error {
				entry, getErr := tx.GetOutbox(ctx, string(attempt.InvocationID))
				if getErr != nil {
					return getErr
				}
				pendingDispatch = !entry.Dispatched()
				return nil
			}); outboxErr != nil {
				return exec.StageResult{}, false, fmt.Errorf("outbox intent: %w", outboxErr)
			}
			if pendingDispatch {
				return exec.StageResult{}, false, nil
			}
		}
		return exec.StageResult{}, false, fmt.Errorf("inspect: %w", err)
	}
	// The inspection is a returned object: validate it before anything
	// trusts its fields, so a driver reporting an impossible pair (a lost
	// session claimed live) fails closed here instead of projecting a lost
	// invocation as observed_live.
	if err := inspection.Validate(); err != nil {
		return exec.StageResult{}, false, fmt.Errorf("inspect: %w", err)
	}
	// Record the paced liveness observation before classifying: what the
	// daemon just saw is observable state whether or not the pass advances
	// (issue #394). Only the mirrored status and live bit cross over.
	if err := e.observeInvocation(ctx, runID, attempt.InvocationID, inspection); err != nil {
		return exec.StageResult{}, false, fmt.Errorf("record observation: %w", err)
	}
	status := inspection.Status
	switch status {
	case exec.StatusPending, exec.StatusRunning:
		return exec.StageResult{}, false, nil
	case exec.StatusCompleted, exec.StatusFailed, exec.StatusCanceled, exec.StatusGone:
		// Collect below. A gone session may still carry a committed result.
	default:
		return exec.StageResult{}, false, fmt.Errorf("inspect returned status %q: %w", status, exec.ErrInvalidStatus)
	}

	result, err := e.driver.Collect(ctx, attempt.InvocationID)
	if err != nil {
		if status == exec.StatusGone && errors.Is(err, exec.ErrNoResult) {
			return exec.StageResult{}, false, fmt.Errorf("%w: %w", ErrInvocationLost, err)
		}
		return exec.StageResult{}, false, fmt.Errorf("collect: %w", err)
	}
	if err := result.Validate(); err != nil {
		return exec.StageResult{}, false, fmt.Errorf("validate collected result: %w", err)
	}
	if result.InvocationID != attempt.InvocationID {
		return exec.StageResult{}, false, fmt.Errorf("collected invocation_id %q, want %q: %w",
			result.InvocationID, attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	if status != exec.StatusGone && result.Status != status {
		return exec.StageResult{}, false, fmt.Errorf("collected status %q disagrees with inspected %q: %w",
			result.Status, status, exec.ErrInvalidStatus)
	}
	if err := appendUsageObservations(ctx, e.store, attempt.InvocationID, result.Usage); err != nil {
		return exec.StageResult{}, false, err
	}
	return result, true, nil
}

func (e *Engine) loadInvocationRequest(ctx context.Context, entry store.QueueEntry) (invocationRequest, invocationBinding, error) {
	request, err := decodeInvocationRequest(entry.Payload)
	if err != nil {
		return invocationRequest{}, invocationBinding{}, err
	}
	if string(request.InvocationID) != entry.IdempotencyKey {
		return invocationRequest{}, invocationBinding{}, fmt.Errorf(
			"payload invocation_id %q disagrees with key %q: %w",
			request.InvocationID, entry.IdempotencyKey, domain.ErrParentKeyMismatch,
		)
	}
	binding, err := e.loadInvocationBinding(ctx, request.InvocationID)
	if err != nil {
		return invocationRequest{}, invocationBinding{}, err
	}
	if *binding.invocation.ConversationID != request.ConversationID ||
		binding.item.ID != request.ItemID || binding.item.ItemVersion != request.ItemVersion {
		return invocationRequest{}, invocationBinding{}, fmt.Errorf(
			"durable invocation binding disagrees with payload: %w", domain.ErrParentKeyMismatch,
		)
	}
	return request, binding, nil
}

// attemptRecorded reports whether the run snapshot already carries an attempt
// for this invocation. It reads the snapshot the pass already loaded rather
// than the store, and recordAttempt re-checks authoritatively inside its own
// transaction, so a concurrent append between the two is still caught.
func attemptRecorded(run domain.Run, invocationID domain.InvocationID) bool {
	for _, stage := range run.Stages {
		for _, attempt := range stage.Attempts {
			if attempt.InvocationID == invocationID {
				return true
			}
		}
	}
	return false
}

// attemptIDFor is the deterministic attempt identity for an invocation: the
// engine derives it rather than generating one, so a replayed dispatch and the
// admission recorded beside it name the same attempt.
func attemptIDFor(invocationID domain.InvocationID) domain.AttemptID {
	return domain.AttemptID("attempt-" + string(invocationID))
}

func (e *Engine) loadInvocationBinding(ctx context.Context, invocationID domain.InvocationID) (invocationBinding, error) {
	var binding invocationBinding
	var names *domain.DisplayNames
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding.invocation, err = tx.GetAgentInvocation(ctx, invocationID)
		if err != nil {
			return err
		}
		if binding.invocation.ConversationID == nil {
			return fmt.Errorf("invocation has no conversation binding: %w", domain.ErrEmptyID)
		}
		binding.conversation, err = tx.GetConversation(ctx, *binding.invocation.ConversationID)
		if err != nil {
			return err
		}
		if binding.invocation.ThroughSequence > len(binding.conversation.Messages) ||
			binding.conversation.Messages[binding.invocation.ThroughSequence-1].Author != domain.AuthorUser {
			return fmt.Errorf("invocation conversation prefix is not present: %w", domain.ErrParentKeyMismatch)
		}

		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		matches := 0
		for _, snapshotted := range items {
			item := snapshotted.Value
			if item.ConversationID == nil || *item.ConversationID != *binding.invocation.ConversationID {
				continue
			}
			matches++
			binding.item = item
		}
		if matches != 1 {
			return fmt.Errorf("conversation %q binds %d attention items, want 1",
				*binding.invocation.ConversationID, matches)
		}
		if binding.item.Subject.RunID == nil || *binding.item.Subject.RunID == "" {
			return fmt.Errorf("attention item %q has no run binding: %w", binding.item.ID, domain.ErrEmptyID)
		}
		binding.run, err = tx.GetRun(ctx, *binding.item.Subject.RunID)
		if err != nil {
			return err
		}
		names, err = tx.DisplayNamesFor(ctx, binding.run.ProjectID, runSubject(binding.run))
		if err != nil {
			return err
		}
		marker, err := tx.GetAttentionItem(ctx, initialItemID(binding.run.ID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: run %q has no fake workflow marker", errForeignWorkflow, binding.run.ID)
			}
			return fmt.Errorf("fake workflow marker for run %q: %w", binding.run.ID, err)
		}
		if !sameWorkflowItem(marker, initialItem(binding.run, names)) {
			return fmt.Errorf("fake workflow marker for run %q disagrees with its binding: %w",
				binding.run.ID, domain.ErrParentKeyMismatch)
		}
		if marker.Status != domain.StatusResolved {
			return fmt.Errorf("fake workflow marker for run %q has status %q, want resolved: %w",
				binding.run.ID, marker.Status, domain.ErrParentKeyMismatch)
		}
		return nil
	})
	if err != nil {
		return invocationBinding{}, err
	}
	if !sameWorkflowItem(binding.item, feedbackItem(binding.run, names)) {
		return invocationBinding{}, fmt.Errorf("attention item %q is not the feedback item for run %q: %w",
			binding.item.ID, binding.run.ID, domain.ErrParentKeyMismatch)
	}
	return binding, nil
}

func completionAlreadyAccepted(conversation domain.Conversation, invocationID domain.InvocationID) (bool, error) {
	wantMessage := domain.MessageID("msg-agent-" + string(invocationID))
	found := false
	for _, message := range conversation.Messages {
		if message.ID == wantMessage {
			if message.Author != domain.AuthorAgent {
				return false, fmt.Errorf("accepted message %q has author %q, want agent",
					wantMessage, message.Author)
			}
			found = true
			break
		}
	}
	// A later discuss may already have returned the conversation to awaiting
	// while this older attempt remains in the append-only Run history. Its own
	// immutable agent message is sufficient proof that this result was accepted.
	if found {
		return true, nil
	}
	switch conversation.Status {
	case domain.ConversationAwaitingAgent:
		return false, nil
	case domain.ConversationIdle:
		return false, fmt.Errorf("conversation is idle without accepted message %q", wantMessage)
	}
	return false, fmt.Errorf("conversation status %q is invalid", conversation.Status)
}

func decodeInvocationRequest(payload []byte) (invocationRequest, error) {
	var request invocationRequest
	if err := strictjson.Decode(payload, &request, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return invocationRequest{}, errors.New("decode payload: trailing JSON value")
		}
		return invocationRequest{}, fmt.Errorf("decode payload: %w", err)
	}
	if request.InvocationID == "" || request.ConversationID == "" || request.ItemID == "" {
		return invocationRequest{}, fmt.Errorf("decode payload: required identity is empty: %w", domain.ErrEmptyID)
	}
	if request.ItemVersion < 1 {
		return invocationRequest{}, fmt.Errorf("decode payload: item_version %d: %w", request.ItemVersion, domain.ErrNonPositive)
	}
	return request, nil
}

// recordAttempt appends the attempt that makes an invocation discoverable
// after a restart and, when the dispatch was admitted, records that admission
// in the same transaction. Splitting the two would leave either an attempt
// with no audited class or an admission for an attempt that was never
// appended, and a crash between them is exactly when the record matters.
func (e *Engine) recordAttempt(
	ctx context.Context, runID domain.RunID, stageID domain.StageID,
	invocationID domain.InvocationID, outboxStatus string, fresh *domain.ExecutionAdmission,
) (bool, domain.ExecutionAdmission, bool, error) {
	added := false
	var effective domain.ExecutionAdmission
	bound := fresh != nil
	if fresh != nil {
		effective = *fresh
	}
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		stageIndex := -1
		for i, stage := range run.Stages {
			for _, attempt := range stage.Attempts {
				if attempt.InvocationID == invocationID {
					if stage.ID != stageID ||
						attempt.ID != attemptIDFor(invocationID) {
						return fmt.Errorf("invocation %q is already bound to attempt %q in stage %q: %w",
							invocationID, attempt.ID, stage.ID, domain.ErrParentKeyMismatch)
					}
					// The attempt is already durable, so this pass must start
					// under whatever admission is stored beside it, never
					// under the one it just built (whose instant, and so
					// whose identity, has moved). The lookup does not depend
					// on this process being configured to admit: an attempt
					// that was admitted stays admitted, and a restart that
					// happens to have lost the configuration must not
					// downgrade it to an unbound start. Reading the record
					// also re-gates it, so a floor raised in the meantime
					// stops the replay here.
					stored, found, err := tx.LookupExecutionAdmission(ctx, invocationID)
					switch {
					case err != nil:
						// Reconstruction was refused, which is not the same as
						// having no record: fail the replay rather than
						// treating a closed gate as a legacy attempt.
						return fmt.Errorf("replayed invocation %q: %w", invocationID, err)
					case found:
						if stored.AuthIdentityID != nil && outboxStatus == "dispatching" && (e.databaseLock == nil || !e.databaseLock.Held()) {
							return ErrDispatchRecoveryUnlocked
						}
						// A recorded admission whose driver never started is
						// still new unattended operation about to begin, so
						// the operating-state gate holds here too (#319): a
						// stop between the record and this dispatch must not
						// leak one last start. The record itself stays
						// readable — this gates the start, not the row.
						if err := tx.RequireUnattendedAdmissible(ctx, stored); err != nil {
							return fmt.Errorf("replayed invocation %q: %w", invocationID, err)
						}
						if err := e.requireReviewConfigurationApproved(ctx, tx, stored); err != nil {
							return fmt.Errorf("replayed invocation %q: %w", invocationID, err)
						}
						if err := tx.RequireIdentityExecutionCapacity(ctx, stored); err != nil {
							return fmt.Errorf("replayed invocation %q: %w", invocationID, err)
						}
						effective, bound = stored, true
					case e.admission == nil:
						// No record exists, so there is no durable decision to
						// honour here and the only question is what to do in
						// its absence. That question, unlike anything about a
						// record that does exist, is legitimately the current
						// configuration's: an engine that admits nothing
						// starts the attempt as it always did, while a
						// configured one falls through and fails closed rather
						// than starting unaudited work.
						bound = false
					default:
						// Configured to admit, but the attempt carries no
						// audited class: fail closed rather than invent one.
						return fmt.Errorf("replayed invocation %q has no admission: %w",
							invocationID, store.ErrNotFound)
					}
					return errReplay
				}
			}
			if stage.ID == stageID {
				stageIndex = i
			}
		}
		if stageIndex < 0 {
			return fmt.Errorf("run %q has no stage %q", runID, stageID)
		}
		if fresh != nil {
			if err := e.requireReviewConfigurationApproved(ctx, tx, *fresh); err != nil {
				return err
			}
		}
		stage := run.Stages[stageIndex]
		stage.Attempts = append(stage.Attempts, domain.Attempt{
			ID:           attemptIDFor(invocationID),
			StageID:      stage.ID,
			Number:       len(stage.Attempts) + 1,
			InvocationID: invocationID,
		})
		run.Stages[stageIndex] = stage
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if fresh != nil {
			if err := tx.RecordExecutionAdmission(ctx, *fresh); err != nil {
				return err
			}
			// §5.7 requires a waived admission to surface its degraded
			// posture, not only to record the waiver. It lands in this same
			// transaction: an admission committed without its notice would
			// leave unattended work running on the exception with nothing
			// telling the operator so.
			if fresh.BackupEncryptionWaiver != nil {
				createdAt := e.admission.now().UTC()
				subject := domain.Subject{
					Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID,
				}
				names, err := tx.DisplayNamesFor(ctx, run.ProjectID, subject)
				if err != nil {
					return err
				}
				item, err := waivedPostureItem(
					run, invocationID, *fresh.BackupEncryptionWaiver, createdAt, names,
				)
				if err != nil {
					return err
				}
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
			}
		}
		added = true
		return nil
	})
	if errors.Is(err, errReplay) {
		return true, effective, bound, nil
	}
	if err != nil {
		return false, domain.ExecutionAdmission{}, false,
			fmt.Errorf("record invocation %q on run %q: %w", invocationID, runID, err)
	}
	return added, effective, bound, nil
}

func (e *Engine) requireReviewConfigurationApproved(
	ctx context.Context, tx *store.WriteTx, admission domain.ExecutionAdmission,
) error {
	if admission.OperatingMode != domain.ModeUnattended {
		return nil
	}
	if e.admission == nil ||
		e.admission.environment.OperatingMode != domain.ModeUnattended {
		// A replay under an unconfigured or attended engine remains governed
		// by its immutable admission record. Such an engine has no live
		// reviewer configuration to compare; the production unattended
		// composition always configures one, which is where this mutable
		// cross-repository gate applies.
		return nil
	}
	return tx.RequireReviewConfigurationApproved(
		ctx, e.admission.environment.ReviewConfigurationDigest,
	)
}

func findFeedbackStage(run domain.Run) (domain.Stage, bool) {
	for _, stage := range run.Stages {
		if stage.ID == feedbackStageID(run.ID) && stage.Name == feedbackStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}
