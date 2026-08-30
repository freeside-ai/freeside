package engine

import (
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func publishBlockFacts(cause domain.RunHoldReason, err error) *domain.PublishBlockFacts {
	switch {
	case errors.Is(err, publish.ErrTrustProfileDrift):
		return publishBlockTrustRule(domain.TrustRuleTrustProfileDrift)
	case errors.Is(err, publish.ErrTargetBaseAdvanced):
		return publishBlockTrustRule(domain.TrustRuleTargetBaseAdvanced)
	default:
		return publishBlockHoldReason(cause)
	}
}

func publishBlockHoldReason(reason domain.RunHoldReason) *domain.PublishBlockFacts {
	return &domain.PublishBlockFacts{HoldReason: &reason}
}

func publishBlockTrustRule(rule domain.TrustRule) *domain.PublishBlockFacts {
	return &domain.PublishBlockFacts{TrustRule: &rule}
}

func definitivePublishBlockFacts(reason string) (*domain.PublishBlockFacts, error) {
	switch reason {
	case productionBlockRecipeRevoked:
		return publishBlockTrustRule(domain.TrustRuleRecipeUnapproved), nil
	case productionBlockVerification:
		return publishBlockTrustRule(domain.TrustRuleVerificationFailed), nil
	case productionBlockTrust:
		return publishBlockTrustRule(domain.TrustRuleTrustProfileDrift), nil
	case productionBlockBaseAdvanced:
		return publishBlockTrustRule(domain.TrustRuleTargetBaseAdvanced), nil
	default:
		return nil, domain.ErrParentKeyMismatch
	}
}
