package engine

import (
	"context"
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Pre-rename vocabulary (#986); the only file in the package that may spell
// it. The store canonicalizes queue kinds and JSON keys on read, so the
// engine's decoders see only the current names. What remains here are the
// idempotency-key and item-ID prefixes the engine mints from a run: a run in
// the legacy identifier family keeps minting legacy keys, so its stored
// claim and quarantine rows stay addressable, and a quarantine notice the
// pre-rename daemon wrote is still recognized as this lane's when released.
const (
	legacySpecificationImplementationClaimKeyPrefix     = "claim-elaboration-implementation-"
	legacySpecificationMarkerQuarantinePrefix           = "elaboration-marker-quarantined-"
	legacySpecificationDiscussionMarkerQuarantinePrefix = "elaboration-discussion-marker-quarantined-"
	legacySpecificationDiscussionInvocationIDPrefix     = "elaboration-discussion-"
	legacySpecificationQuarantineUnreadable             = "A stored elaboration marker could not be authenticated. " +
		"The run is held out of the elaboration lane, and resumes by itself once the marker reconstructs again."
)

// specificationImplementationClaimKey names the outbox row that reserves the
// implementation identity. The implementation run ID carries no family
// marker, so the specification run selects the family.
func specificationImplementationClaimKey(specificationRunID, implementationRunID domain.RunID) string {
	if domain.LegacySpecificationRun(specificationRunID) {
		return legacySpecificationImplementationClaimKeyPrefix + string(implementationRunID)
	}
	return specificationImplementationClaimKeyPrefix + string(implementationRunID)
}

// getSpecificationImplementationClaim reads the claim that reserved an
// implementation run when only that run's identity is known: the claim was
// written by the specification run derived from it in one family or the
// other, so both keys are tried before reporting store.ErrNotFound.
func getSpecificationImplementationClaim(
	ctx context.Context, tx *store.WriteTx, implementationRunID domain.RunID,
) (store.QueueEntry, error) {
	claim, err := tx.GetOutbox(ctx, specificationImplementationClaimKey(
		domain.SpecificationRunIDForImplementation(implementationRunID), implementationRunID))
	if !errors.Is(err, store.ErrNotFound) {
		return claim, err
	}
	return tx.GetOutbox(ctx, specificationImplementationClaimKey(
		domain.LegacySpecificationRunIDForImplementation(implementationRunID), implementationRunID))
}

// specificationDiscussionMarkerIdentity returns the identity an existing
// discussion marker for the command already carries, in either family, and
// the current-family identity when no marker exists yet. Unlike the run-
// derived identities above, a discussion identity derives from a command ID
// that carries no family marker, so the family can only be read back from
// the stored row. A stored legacy marker wins over a current-family row for
// the same command: only the pre-rename daemon minted legacy keys, so when
// both exist the legacy row is the original and the current-family row is a
// twin a daemon built before #1100 minted beside it. The answer cannot go
// stale under the enqueue that follows: the pre-rename daemon is not
// running, and the current-family key stays covered by EnqueueOutbox's own
// dedup.
func specificationDiscussionMarkerIdentity(
	ctx context.Context, tx *store.ReadTx, commandID string,
) (domain.InvocationID, error) {
	legacy := domain.InvocationID(legacySpecificationDiscussionInvocationIDPrefix + commandID)
	switch _, err := tx.GetOutbox(ctx, string(legacy)); {
	case err == nil:
		return legacy, nil
	case errors.Is(err, store.ErrNotFound):
		return specDiscussionInvocationID(commandID), nil
	default:
		return "", err
	}
}

// retireSupersededSpecDiscussionMarker takes a queued current-family
// discussion marker out of the start loop when the legacy marker for the
// same command owns the identity. A daemon built between the rename (#986)
// and #1100 minted that twin beside the legacy row, and both rows are
// startable: leaving the twin queued starts a second provider invocation for
// one accepted Discuss command, and the loser's completion ends the
// reconcile pass with ErrParentKeyMismatch. The twin is marked dispatched
// because that is the only way the engine can retire a queued row, and this
// intent must never be delivered.
//
// Only a twin that never crossed the queue boundary is retired. An attempt
// already on the run means the older daemon recorded it before the dispatch
// mark, so ListPendingOutbox still returns the row while
// acceptCompletedInvocations owns the attempt. Retiring the row there would
// hide it from the recorded-attempt recovery the start loop runs next
// without releasing the attempt, so the run keeps an attempt no dispatch
// backs. Such a twin is left exactly as an older daemon left it.
func (e *Engine) retireSupersededSpecDiscussionMarker(
	ctx context.Context, entry store.QueueEntry, run domain.Run, invocationID domain.InvocationID,
) (bool, error) {
	commandID, ok := domain.SpecificationDiscussionCommandID(entry.IdempotencyKey)
	if !ok || domain.InvocationID(entry.IdempotencyKey) != specDiscussionInvocationID(commandID) {
		return false, nil
	}
	if attemptRecorded(run, invocationID) {
		return false, nil
	}
	var identity domain.InvocationID
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		identity, err = specificationDiscussionMarkerIdentity(ctx, tx, commandID)
		return err
	}); err != nil {
		return false, err
	}
	if identity == domain.InvocationID(entry.IdempotencyKey) {
		return false, nil
	}
	return true, e.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey)
	})
}

// The quarantine notice prefixes follow the run's family on both the record
// and the release side, so a hold written for a legacy run is found again
// when its marker reconstructs.
func specificationMarkerQuarantinePrefixFor(runID domain.RunID) string {
	if domain.LegacySpecificationRun(runID) {
		return legacySpecificationMarkerQuarantinePrefix
	}
	return specificationMarkerQuarantinePrefix
}

func specificationDiscussionMarkerQuarantinePrefixFor(runID domain.RunID) string {
	if domain.LegacySpecificationRun(runID) {
		return legacySpecificationDiscussionMarkerQuarantinePrefix
	}
	return specificationDiscussionMarkerQuarantinePrefix
}

// legacySpecificationQuarantineNoticeFor recognizes a specification marker
// hold under the legacy prefix, whether the pre-rename daemon wrote its
// reason or the current one did.
func legacySpecificationQuarantineNoticeFor(prefix, reason string) bool {
	return prefix == legacySpecificationMarkerQuarantinePrefix &&
		(reason == specificationQuarantineUnreadable || reason == legacySpecificationQuarantineUnreadable)
}
