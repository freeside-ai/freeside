package domain

import (
	"errors"
	"fmt"
)

// HandoffCheck is the handoff proof of one publication (plan §6, §7,
// revision 110; issue #2644). At handoff the workflow audit resolves the
// target's tip. When the tip is the base the run's evidence binds, nothing
// else is needed. When it moved, the daemon builds the prospective merge of
// the unchanged head into that tip in its own checkout, never pushes it,
// verifies it under the admitted recipe, and records the merge and the proof
// of that verification here. Review is not rerun: the head is unchanged.
//
// The store seals the tip first and never re-resolves it for the
// publication, so an advance while the merge is verified, or before the
// publication lands, does not restart the check. Merge is sealed once,
// after verification passed.
//
// The publication decision (publish.storePublicationDecision) re-checks the
// sealed record against the candidate, its authorization, its readiness
// proofs, and the merge's check proof; the engine's copy on the candidate is
// a pointer, never authority.
type HandoffCheck struct {
	RunID         RunID        `json:"run_id"`
	PublicationID InvocationID `json:"publication_id"`
	HeadSHA       string       `json:"head_sha"`
	// EvidenceBaseSHA is the base the run's verification, review, and
	// readiness proofs bind: the admission base.
	EvidenceBaseSHA string `json:"evidence_base_sha"`
	// TipSHA is the target tip the handoff audit resolved.
	TipSHA string `json:"tip_sha"`
	// WorkflowAuditID is the workflow_audits row TipSHA was sealed from: an
	// audit of the run's repository whose AuditedRef is the target ref and
	// whose AuditedCommitSHA is TipSHA. The seal and the publication decision
	// both re-check that row, so the store, not the engine's copy, proves
	// the tip was observed on the run's own target. Zero on checks sealed
	// before migration 0129; Validate accepts zero so recovery still decodes
	// them, and the publication decision refuses them.
	WorkflowAuditID int64 `json:"workflow_audit_id"`
	// Merge is the verified merge of HeadSHA into TipSHA, nil until verified
	// and always nil while TipSHA is EvidenceBaseSHA.
	Merge *HandoffMerge `json:"merge"`
}

// HandoffMerge is the verified prospective merge a moved tip requires.
type HandoffMerge struct {
	MergeSHA string `json:"merge_sha"`
	// VerificationProofDigest names the CheckProof, under the run's
	// clean-verification resolution and the admitted recipe, that the merge
	// passed verification.
	VerificationProofDigest Digest `json:"verification_proof_digest"`
}

// ErrHandoffCheckInconsistent marks a handoff check whose merge contradicts
// its tip and evidence base.
var ErrHandoffCheckInconsistent = errors.New("handoff check merge contradicts its tip and evidence base")

// Validate checks the record's own shape: every coordinate present, and a
// merge only for a moved tip, naming three distinct commits.
func (c HandoffCheck) Validate() error {
	if c.RunID == "" || c.PublicationID == "" || c.HeadSHA == "" ||
		c.EvidenceBaseSHA == "" || c.TipSHA == "" {
		return fmt.Errorf("handoff check: %w", ErrEmptyField)
	}
	if c.Merge == nil {
		return nil
	}
	if !c.Moved() {
		return fmt.Errorf("handoff check names a merge for an unchanged tip: %w", ErrHandoffCheckInconsistent)
	}
	if c.Merge.VerificationProofDigest == "" {
		return fmt.Errorf("handoff check merge proof: %w", ErrEmptyField)
	}
	return c.MergeIdentity().Validate()
}

// Moved reports whether the tip differs from the base the evidence binds.
func (c HandoffCheck) Moved() bool { return c.TipSHA != c.EvidenceBaseSHA }

// Proved reports whether the record by itself satisfies the handoff: the tip
// is unchanged, or a verified merge into it is recorded. The publication
// decision still re-checks the merge's proof.
func (c HandoffCheck) Proved() bool { return !c.Moved() || c.Merge != nil }

// MergeIdentity is the merge of the head into the tip the record names. Its
// MergeSHA is empty until the merge is recorded.
func (c HandoffCheck) MergeIdentity() ProspectiveMergeIdentity {
	identity := ProspectiveMergeIdentity{BaseSHA: c.TipSHA, HeadSHA: c.HeadSHA}
	if c.Merge != nil {
		identity.MergeSHA = c.Merge.MergeSHA
	}
	return identity
}
