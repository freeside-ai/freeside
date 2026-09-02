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
