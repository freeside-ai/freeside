package engine

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestHandoffBlockTextsNameTheBaseAdvanceRule: every text a base-advance
// block carries, the handoff's (#2644) and the legacy re-entry texts a stored
// block may still name, maps to the target_base_advanced trust rule, so a
// recovered block keeps its facts.
func TestHandoffBlockTextsNameTheBaseAdvanceRule(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{
		productionBlockBaseAdvanced,
		productionBlockHandoffConflict,
		productionBlockHandoffVerification,
		productionBlockReentryRetired,
		productionBlockReentryConflict,
		productionBlockReentryVerification,
		productionBlockReentryRoundLimit,
	} {
		facts, err := definitivePublishBlockFacts(reason)
		if err != nil {
			t.Fatalf("%q: %v", reason, err)
		}
		if facts.TrustRule == nil || *facts.TrustRule != domain.TrustRuleTargetBaseAdvanced || facts.HoldReason != nil {
			t.Errorf("%q facts = %+v, want the target_base_advanced rule", reason, facts)
		}
	}
}
