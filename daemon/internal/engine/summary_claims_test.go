package engine

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func summaryClaimFixture(invocationID domain.InvocationID, content string) domain.AgentClaim {
	text := domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: content}
	return domain.AgentClaim{
		Label: export.SummaryEvidenceLabel, Artifact: domain.ArtifactID("summary-" + invocationID),
		Digest: text.ComputeDigest(), Text: &text,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivitySensitive,
		},
	}
}

func TestNormalizeSummaryClaimsRequiresOneInvocationBoundMarkdownClaim(t *testing.T) {
	invocationID := domain.InvocationID("inv-summary")
	valid := summaryClaimFixture(invocationID, "Changed the import path; one decision remains open.")
	ordinary := domain.AgentClaim{
		Label: "screenshot", Artifact: "shot", Digest: "sha256:shot",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}

	got := normalizeSummaryClaims([]domain.AgentClaim{ordinary, valid}, invocationID)
	if got[1].Text == nil {
		t.Fatal("one valid summary lost its inline text")
	}

	foreign := valid
	foreign.Provenance.ProducerInvocationID = "inv-other"
	got = normalizeSummaryClaims([]domain.AgentClaim{ordinary, foreign}, invocationID)
	if got[1].Text != nil || got[0].Label != ordinary.Label {
		t.Fatalf("misbound normalization = %+v", got)
	}

	duplicate := summaryClaimFixture(invocationID, "A second summary.")
	got = normalizeSummaryClaims([]domain.AgentClaim{valid, duplicate}, invocationID)
	if got[0].Text != nil || got[1].Text != nil {
		t.Fatalf("duplicate summaries retained inline authority: %+v", got)
	}
}
