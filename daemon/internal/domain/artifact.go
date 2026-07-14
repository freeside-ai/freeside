package domain

import "fmt"

// Provenance is the machine-checkable origin of an artifact (plan §5.15 rule
// 2). VerificationRecipeDigest is set only for artifacts a recipe produced; it
// is nil for agent output.
type Provenance struct {
	ProducerClass            ProducerClass    `json:"producer_class"`
	ProducerInvocationID     InvocationID     `json:"producer_invocation_id"`
	SourceHeadSHA            string           `json:"source_head_sha"`
	VerificationRecipeDigest *Digest          `json:"verification_recipe_digest"`
	SensitivityClass         SensitivityClass `json:"sensitivity_class"`
}

// Validate reports whether the provenance is well-formed.
func (p Provenance) Validate() error {
	if !p.ProducerClass.valid() {
		return fmt.Errorf("provenance producer_class %q: %w", p.ProducerClass, ErrInvalidProducerClass)
	}
	if p.ProducerInvocationID == "" {
		return fmt.Errorf("provenance producer_invocation_id: %w", ErrEmptyID)
	}
	// source_head_sha binds an artifact to the revision that produced it; a
	// remediation head invalidates prior-head evidence, and the publisher
	// verifies head binding before publication (plan §5.15 rule 2). It is a
	// non-optional provenance field, so an empty value is rejected here rather
	// than surfacing as unbindable evidence downstream.
	if p.SourceHeadSHA == "" {
		return fmt.Errorf("provenance source_head_sha: %w", ErrEmptyField)
	}
	if !p.SensitivityClass.valid() {
		return fmt.Errorf("provenance sensitivity_class %q: %w", p.SensitivityClass, ErrInvalidSensitivityClass)
	}
	// The recipe digest is optional (nil for non-recipe artifacts), but a
	// present one is a content address, so an empty string behind the pointer
	// is malformed, not "absent".
	if p.VerificationRecipeDigest != nil && *p.VerificationRecipeDigest == "" {
		return fmt.Errorf("provenance verification_recipe_digest: %w", ErrEmptyField)
	}
	// Agent output is never produced under a verification recipe (plan §5.15),
	// so a recipe digest on an agent artifact is a machine-checkable falsehood.
	if p.ProducerClass == ProducerAgent && p.VerificationRecipeDigest != nil {
		return fmt.Errorf("agent artifact with a recipe digest: %w", ErrProvenanceInconsistent)
	}
	return nil
}

// clone returns a copy whose recipe-digest pointer is detached from the
// caller's, so a stored provenance cannot change when the caller reuses the
// Digest variable it passed in.
func (p Provenance) clone() Provenance {
	p.VerificationRecipeDigest = clonePtr(p.VerificationRecipeDigest)
	return p
}

// Artifact is a typed, immutable, digest-addressed input or output (plan §5.15).
// PublishEligible is exported so the type serializes, but it is computed by
// trusted policy in NewArtifact and never taken from caller input; see
// ArtifactInput.
type Artifact struct {
	ID              ArtifactID `json:"id"`
	Type            string     `json:"type"`
	Digest          Digest     `json:"digest"`
	Provenance      Provenance `json:"provenance"`
	PublishEligible bool       `json:"publish_eligible"`
}

// Validate reports whether the artifact is well-formed. It is the backstop for
// artifacts that never went through NewArtifact (reconstructed from the store
// or built as a literal): it rejects a PublishEligible value inconsistent with
// provenance as far as is checkable without the approved-recipe set. An agent
// artifact is never eligible, and eligibility needs a recipe digest; the
// remaining "is the recipe approved" half is recomputed by NewArtifact against
// policy, since Validate has no policy to check against.
func (a Artifact) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("artifact id: %w", ErrEmptyID)
	}
	if a.Type == "" {
		return fmt.Errorf("artifact %s type: %w", a.ID, ErrEmptyField)
	}
	if a.Digest == "" {
		return fmt.Errorf("artifact %s digest: %w", a.ID, ErrEmptyField)
	}
	if err := a.Provenance.Validate(); err != nil {
		return err
	}
	if a.PublishEligible {
		switch a.Provenance.ProducerClass {
		case ProducerVerifier, ProducerDaemon:
			if a.Provenance.VerificationRecipeDigest == nil {
				return fmt.Errorf("artifact %s publish_eligible without a recipe: %w", a.ID, ErrPublishEligibleInconsistent)
			}
		case ProducerAgent:
			return fmt.Errorf("artifact %s: agent artifact cannot be publish_eligible: %w", a.ID, ErrPublishEligibleInconsistent)
		}
	}
	return nil
}

// ValidatePublishEligibility asserts a standalone artifact's persisted
// PublishEligible equals the policy computation over approvedRecipes.
// PublishEligible is computed by trusted policy (NewArtifact), never supplied,
// so a stored or exported value that disagrees, a forged or stale bit, is
// rejected. It is the persistence/reconstruction boundary check for artifact
// rows the store did not build through NewArtifact: unlike
// EligibleForEvidenceSnapshot, it does not require the artifact to be evidence
// (an agent artifact with PublishEligible false is a legal standalone row), it
// only forbids a bit that policy would not have produced. Self-standing: it
// re-runs structural Validate first, so a caller re-checking a reconstructed
// row cannot admit a value the validator would reject.
func ValidatePublishEligibility(a Artifact, approvedRecipes map[Digest]bool) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.PublishEligible != computePublishEligible(a.Provenance, approvedRecipes) {
		return fmt.Errorf("artifact %s: %w", a.ID, ErrPublishEligibleInconsistent)
	}
	return nil
}

// clone returns a deep copy detached from any caller-owned pointer, so a
// validated artifact cannot change when the caller mutates the provenance it
// passed in.
func (a Artifact) clone() Artifact {
	a.Provenance = a.Provenance.clone()
	return a
}

// cloneArtifacts deep-copies a slice of artifacts, preserving nil.
func cloneArtifacts(in []Artifact) []Artifact {
	if in == nil {
		return nil
	}
	out := make([]Artifact, len(in))
	for i, a := range in {
		out[i] = a.clone()
	}
	return out
}

// ArtifactInput carries the caller-supplied fields of an Artifact. It has no
// PublishEligible field: publish eligibility is computed by trusted policy, so
// there is no input path, agent-supplied or otherwise, that can set it (plan
// §5.15 rule 2).
type ArtifactInput struct {
	ID         ArtifactID
	Type       string
	Digest     Digest
	Provenance Provenance
}

// NewArtifact builds a validated Artifact, computing PublishEligible from the
// provenance and the approved-recipe set. The flag can only originate here.
func NewArtifact(in ArtifactInput, approvedRecipes map[Digest]bool) (Artifact, error) {
	a := Artifact{
		ID:         in.ID,
		Type:       in.Type,
		Digest:     in.Digest,
		Provenance: in.Provenance.clone(),
	}
	if err := a.Validate(); err != nil {
		return Artifact{}, err
	}
	a.PublishEligible = computePublishEligible(a.Provenance, approvedRecipes)
	return a, nil
}

// computePublishEligible implements the publish-eligibility policy (plan §5.15
// rule 2): only a verifier or daemon artifact produced under an approved recipe
// is eligible; agent artifacts never are. It is unexported so only trusted
// construction reaches it. The switch omits default so a new ProducerClass must
// be handled here (exhaustive lint); the trailing return covers the invalid
// zero value.
func computePublishEligible(p Provenance, approvedRecipes map[Digest]bool) bool {
	switch p.ProducerClass {
	case ProducerVerifier, ProducerDaemon:
		return p.VerificationRecipeDigest != nil && approvedRecipes[*p.VerificationRecipeDigest]
	case ProducerAgent:
		return false
	}
	return false
}

// EligibleForEvidenceSnapshot reports whether an artifact may enter an item's
// evidence snapshot (plan §5.15 rule 2). A verifier/daemon artifact under an
// approved recipe is admitted; an agent artifact is refused (it belongs in a
// labeled AgentClaim); anything else is an invalid producer class. The switch
// omits default for exhaustive lint; the trailing return covers the invalid
// zero value.
func EligibleForEvidenceSnapshot(a Artifact, approvedRecipes map[Digest]bool) error {
	// The trust decision presupposes a well-formed artifact: a caller invoking
	// the gate directly (e.g. re-checking a store-reconstructed item) must not
	// admit evidence the artifact validator would reject.
	if err := a.Validate(); err != nil {
		return err
	}
	switch a.Provenance.ProducerClass {
	case ProducerVerifier, ProducerDaemon:
		if a.Provenance.VerificationRecipeDigest == nil || !approvedRecipes[*a.Provenance.VerificationRecipeDigest] {
			return fmt.Errorf("evidence artifact %s: %w", a.ID, ErrUnapprovedRecipe)
		}
		// The gate holds the policy, so it is the one place that can catch a
		// stale publish_eligible in *either* direction (an approved artifact
		// that reads not-publishable, as well as the inverse Validate already
		// rejects). A reconstructed item re-running this gate is thereby forced
		// to carry the policy-computed bit, not a decoded stale one.
		if a.PublishEligible != computePublishEligible(a.Provenance, approvedRecipes) {
			return fmt.Errorf("evidence artifact %s publish_eligible stale: %w", a.ID, ErrPublishEligibleInconsistent)
		}
		return nil
	case ProducerAgent:
		return fmt.Errorf("evidence artifact %s: %w", a.ID, ErrAgentArtifactInEvidence)
	}
	return fmt.Errorf("evidence artifact %s producer_class %q: %w", a.ID, a.Provenance.ProducerClass, ErrInvalidProducerClass)
}
