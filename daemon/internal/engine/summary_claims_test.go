package engine

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// evidenceMetaTime is the fixed, UTC creation time used by the evidence
// metadata test helpers below so constructed evidence passes Validate.
var evidenceMetaTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// runMeta returns valid run-sourced evidence metadata for artifact fixtures.
func runMeta() domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1, CreatedAt: evidenceMetaTime,
		Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
	}
}

// claimMeta returns valid claim-sourced evidence metadata whose media type
// matches the claim's inline text (or the caller's supplied type).
func claimMeta(mt domain.EvidenceMediaType) domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType: mt, SizeBytes: 1, CreatedAt: evidenceMetaTime,
		Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
	}
}

// claimTextMeta is valid claim-source metadata for an inline text claim: it
// derives both the media type and size_bytes from the content, so the fixture
// satisfies AgentClaim.Validate's text/metadata bindings by construction.
func claimTextMeta(text domain.ClaimText) domain.EvidenceMetadata {
	m := claimMeta(domain.EvidenceMediaType(text.MediaType))
	m.SizeBytes = int64(len(text.Content))
	return m
}

func summaryClaimFixture(invocationID domain.InvocationID, content string) domain.AgentClaim {
	text := domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: content}
	return domain.AgentClaim{
		Label: export.SummaryEvidenceLabel, Artifact: domain.ArtifactID("summary-" + invocationID),
		Digest: text.ComputeDigest(), Text: &text,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivitySensitive,
		},
		Metadata: claimTextMeta(text),
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
		Metadata: claimMeta(domain.EvidenceMediaImagePNG),
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
