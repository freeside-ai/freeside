package engine

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// BackupPayloadExtractors returns the registry the encrypted checkpoint's
// producer and health evaluator use to read each outbox kind's blob
// references. It is the one definition: the daemon and the real-run verifier
// both read it, because a verifier missing a kind the daemon writes cannot
// prove the checkpoint's closure and reads it as unhealthy (#2053).
func BackupPayloadExtractors() map[string]store.BackupPayloadDigestExtractor {
	return map[string]store.BackupPayloadDigestExtractor{
		FakePublicationTaskKind:                     FakePublicationBackupPayloadDigests,
		FakePublicationInvocationOwnerKind:          FakePublicationInvocationOwnerBackupPayloadDigests,
		signet.AgentInvocationRequestedKind:         signet.AgentInvocationBackupPayloadDigests,
		signet.PublicationReevaluationRequestedKind: signet.PublicationReevaluationBackupPayloadDigests,
		signet.PublicationReevaluationCompletedKind: signet.PublicationReevaluationCompletionBackupPayloadDigests,
		KindProductionInvocationRequested:           ProductionInvocationBackupPayloadDigests,
		KindProductionPublicationRequested:          ProductionPublicationBackupPayloadDigests,
		KindProductionClosureWait:                   ProductionClosureWaitBackupPayloadDigests,
		KindRemediationInvocationRequested:          RemediationInvocationBackupPayloadDigests,
		KindOperatorFeedbackInvocationRequested:     OperatorFeedbackInvocationBackupPayloadDigests,
		domain.PublicationSuccessorKind:             PublicationSuccessorBackupPayloadDigests,
		domain.PublicationContinuationRequestedKind: PublicationContinuationBackupPayloadDigests,
		KindSpecificationInvocationRequested:        SpecificationInvocationBackupPayloadDigests,
		KindSpecificationDiscussionRequested:        SpecificationDiscussionBackupPayloadDigests,
		KindSpecificationImplementationClaim:        SpecificationImplementationClaimBackupPayloadDigests,
		publish.ExternalReplyIntentKind:             publish.ExternalReplyBackupPayloadDigests,
		publish.ExternalReplyOutcomeKind:            publish.ExternalReplyBackupPayloadDigests,
		publish.IntentKindReservation:               publish.ReservationBackupPayloadDigests,
		publish.IntentKindPublication:               publish.PublicationBackupPayloadDigests,
	}
}
