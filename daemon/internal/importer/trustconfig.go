package importer

import (
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// WithProtectedPaths returns a copy of the policy widened by the repository's
// trust-anchored protected-path configuration (plan §5.5, §5.8): the seven
// per-category widening lists the human-approved AutomationTrustProfile
// carries. It is the trust boundary between the content-addressed profile and
// the flat import policy, and it fails closed — profile.Validate recomputes
// the profile's content digest and validates its ProtectedPaths, so an absent
// (zero-value) or tampered profile is rejected rather than silently importing
// with no control-plane widening. The returned globs are still re-checked by
// Options.validate, so a malformed pattern also fails closed at import.
//
// The commit-plan mode, message-ruleset identifier, and automation-change
// stance are copied with the Extra* path fields; caps and the allowlist stay
// untouched. The stance is recorded only when it is the advisory opt-in: block
// is the policy's zero value, so a block profile's options keep the bytes they
// had before the stance crossed this boundary and a recorded replay still
// matches the options rebuilt from its bound profile. The profile is the
// trust-anchored source, so any prior profile-derived values on the policy are
// replaced rather than merged.
//
// Category crosswalk: the profile's ExtraVerificationControlPatterns feeds this
// package's verification_recipes gate (Policy.ExtraVerificationRecipePatterns /
// FindingVerificationRecipePath). The domain field name predates the §5.8
// category vocabulary; its category is verification_recipes. This is the
// import-stage control-plane class and is NOT the verify package's separate
// verify-stage verification_control_path risk-flag — do not wire the two
// together.
func (p Policy) WithProtectedPaths(profile domain.AutomationTrustProfile) (Policy, error) {
	if err := profile.Validate(); err != nil {
		return Policy{}, fmt.Errorf("protected paths from trust profile: %w: %w", err, ErrInvalidOptions)
	}
	// Clone each list while crossing the boundary so the policy holds the
	// validated snapshot, not a live alias of the profile's backing arrays: a
	// caller mutating the profile in place after this call (a valid glob edit
	// re-runs no digest check and still passes Options.validate) must not be
	// able to narrow or redirect control-plane coverage out of band. slices.Clone
	// preserves nil for an empty list.
	cfg := profile.ProtectedPaths
	p.CommitPlan = profile.CommitPlan
	p.MessageRuleset = profile.MessageRuleset
	p.CandidateAutomationChanges = ""
	if profile.CandidateAutomationChanges != domain.AutomationChangesBlocked {
		p.CandidateAutomationChanges = profile.CandidateAutomationChanges
	}
	p.ExtraAutomationControlPatterns = slices.Clone(cfg.ExtraAutomationControlPatterns)
	p.ExtraReviewerInstructionPatterns = slices.Clone(cfg.ExtraReviewerInstructionPatterns)
	p.ExtraGitMetadataPatterns = slices.Clone(cfg.ExtraGitMetadataPatterns)
	p.ExtraVerificationRecipePatterns = slices.Clone(cfg.ExtraVerificationControlPatterns)
	p.ExtraPromptsPolicyPatterns = slices.Clone(cfg.ExtraPromptsAndPolicyPatterns)
	p.ExtraEgressTrustPatterns = slices.Clone(cfg.ExtraEgressAndTrustPatterns)
	p.ExtraMaterialityRulesPatterns = slices.Clone(cfg.ExtraMaterialityRulesPatterns)
	return p, nil
}
