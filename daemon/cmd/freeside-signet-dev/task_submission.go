package main

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

// The convergence harness accepts submissions but runs no engine dispatcher.
// Its fixed test project exercises the real transaction and approval boundary
// without starting an agent or publishing anything.
func convergenceManualInitiator(project domain.ProjectID) (engine.ManualInitiator, bool) {
	if project != "submission-convergence" {
		return engine.ManualInitiator{}, false
	}
	provenance := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: convergenceDigest("submission-policy")}
	keys := []domain.PolicyKey{}
	for _, entry := range [][2]string{
		{specify.PolicySpecApproval, "true"},
		{specify.PolicyMaxIterations, "4"},
		{specify.PolicyStageActiveTime, "1m"},
		{specify.PolicyApprovalWait, "1m"},
		{specify.PolicyResearchAllowlist, "https://example.com"},
		{specify.PolicyResearchMaxBytes, "1024"},
		{"paths", "src/**"},
	} {
		keys = append(keys, domain.PolicyKey{Key: entry[0], Value: entry[1], Provenance: provenance})
	}
	return engine.ManualInitiator{PolicyKeys: keys, CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "convergence", BotUserID: 1}}, true
}
