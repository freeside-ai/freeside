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
// Only the Extra* fields are set; the caps, the allowlist, and the accessors'
// mandatory defaults are untouched, so config can only widen a gate, never
// narrow or disable one. The profile is the trust-anchored source, so any
// widening already on the policy is replaced, not merged.
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
	p.ExtraAutomationControlPatterns = slices.Clone(cfg.ExtraAutomationControlPatterns)
	p.ExtraReviewerInstructionPatterns = slices.Clone(cfg.ExtraReviewerInstructionPatterns)
	p.ExtraGitMetadataPatterns = slices.Clone(cfg.ExtraGitMetadataPatterns)
	p.ExtraVerificationRecipePatterns = slices.Clone(cfg.ExtraVerificationControlPatterns)
	p.ExtraPromptsPolicyPatterns = slices.Clone(cfg.ExtraPromptsAndPolicyPatterns)
	p.ExtraEgressTrustPatterns = slices.Clone(cfg.ExtraEgressAndTrustPatterns)
	p.ExtraMaterialityRulesPatterns = slices.Clone(cfg.ExtraMaterialityRulesPatterns)
	return p, nil
}
