package engine_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

func TestBackupPayloadExtractorsIncludeWorkflowMarkers(t *testing.T) {
	t.Parallel()
	extractors := engine.BackupPayloadExtractors()
	for _, kind := range []string{
		publish.ExternalReplyIntentKind,
		publish.ExternalReplyOutcomeKind,
		engine.KindProductionClosureWait,
		engine.KindSpecificationInvocationRequested,
		engine.KindSpecificationImplementationClaim,
		engine.KindRemediationInvocationRequested,
		engine.KindOperatorFeedbackInvocationRequested,
		signet.PublicationReevaluationRequestedKind,
		signet.PublicationReevaluationCompletedKind,
	} {
		if extractors[kind] == nil {
			t.Errorf("backup payload extractor %q is not registered", kind)
		}
	}
}
