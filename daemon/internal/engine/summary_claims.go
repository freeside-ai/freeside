package engine

import (
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// summaryClaimForInvocation returns the one renderable summary claim produced
// by the named stage invocation. A duplicate, malformed, artifact-only, or
// misbound reserved claim has no summary authority.
func summaryClaimForInvocation(
	claims []domain.AgentClaim, invocationID domain.InvocationID,
) (domain.AgentClaim, bool) {
	matched := -1
	for i := range claims {
		if claims[i].Label != export.SummaryEvidenceLabel {
			continue
		}
		if matched >= 0 {
			return domain.AgentClaim{}, false
		}
		matched = i
	}
	if matched < 0 {
		return domain.AgentClaim{}, false
	}
	claim := claims[matched]
	if claim.Validate() != nil || claim.Text == nil ||
		claim.Text.MediaType != domain.MediaTypeTextMarkdown ||
		claim.Provenance.ProducerInvocationID != invocationID {
		return domain.AgentClaim{}, false
	}
	return claim, true
}

// normalizeSummaryClaims preserves every claim and digest row, but removes
// inline prose from reserved claims that cannot hold the one summary role for
// this invocation. Clients route only a reserved inline claim into layer 2, so
// the remaining artifact reference stays visible as an ordinary claim without
// rendering unauthenticated summary prose.
func normalizeSummaryClaims(
	claims []domain.AgentClaim, invocationID domain.InvocationID,
) []domain.AgentClaim {
	if len(claims) == 0 {
		return claims
	}
	if _, ok := summaryClaimForInvocation(claims, invocationID); ok {
		return claims
	}
	normalized := append([]domain.AgentClaim(nil), claims...)
	for i := range normalized {
		if normalized[i].Label == export.SummaryEvidenceLabel {
			normalized[i].Text = nil
		}
	}
	return normalized
}
