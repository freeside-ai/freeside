package domain

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ProspectiveMerge is the exact merge a closure approval is checked against: the
// publication identity, the candidate head, and the base the pull request
// targets. It is what the daemon observes now, against which ClosureApproval
// records what the owner approved. The base SHA is carried explicitly because
// publish.IdentityInput binds the base ref but not the base SHA, so a base
// advance under the same ref would otherwise go unnoticed (#1417, PR #1420
// review round 10).
type ProspectiveMerge struct {
	PublicationIdentity Digest `json:"publication_identity"`
	CandidateHeadSHA    string `json:"candidate_head_sha"`
	BaseRef             string `json:"base_ref"`
	BaseSHA             string `json:"base_sha"`
}

// Validate reports whether the prospective merge is well-formed.
func (m ProspectiveMerge) Validate() error {
	if !contentaddr.Valid(string(m.PublicationIdentity)) {
		return fmt.Errorf("prospective merge publication_identity %q: %w", m.PublicationIdentity, ErrClosureApprovalInconsistent)
	}
	for name, v := range map[string]string{
		"candidate_head_sha": m.CandidateHeadSHA, "base_ref": m.BaseRef, "base_sha": m.BaseSHA,
	} {
		if v == "" {
			return fmt.Errorf("prospective merge %s: %w", name, ErrClosureApprovalInconsistent)
		}
	}
	return nil
}

// ClosureApproval is what one owner approval of a source-issue-closure proposal
// covers. The proposal artifact does not depend on the candidate head or base,
// so the approval carries them itself. Any change to the head, the base (ref or
// SHA), or the publication identity supersedes the approval; that is the
// closure-side of the §7 rule that a prospective-merge change invalidates prior
// review and verification.
type ClosureApproval struct {
	ProposalDigest      Digest `json:"proposal_digest"`
	PublicationIdentity Digest `json:"publication_identity"`
	CandidateHeadSHA    string `json:"candidate_head_sha"`
	BaseRef             string `json:"base_ref"`
	BaseSHA             string `json:"base_sha"`
}

// Validate reports whether the binding is well-formed.
func (a ClosureApproval) Validate() error {
	if !contentaddr.Valid(string(a.ProposalDigest)) {
		return fmt.Errorf("closure approval proposal_digest %q: %w", a.ProposalDigest, ErrClosureApprovalInconsistent)
	}
	if !contentaddr.Valid(string(a.PublicationIdentity)) {
		return fmt.Errorf("closure approval publication_identity %q: %w", a.PublicationIdentity, ErrClosureApprovalInconsistent)
	}
	for name, v := range map[string]string{
		"candidate_head_sha": a.CandidateHeadSHA, "base_ref": a.BaseRef, "base_sha": a.BaseSHA,
	} {
		if v == "" {
			return fmt.Errorf("closure approval %s: %w", name, ErrClosureApprovalInconsistent)
		}
	}
	return nil
}

// AuthorizesClose reports whether this approval still authorizes closing the
// source issue for the given proposal and current prospective merge. It fails
// closed: an ill-formed approval, proposal, or merge, a non-closure or
// non-resolving proposal, or any of the five bound values diverging from the
// current prospective merge yields false. No approval means no close is the
// caller's concern; this function only judges a present approval.
func (a ClosureApproval) AuthorizesClose(proposal EffectProposal, merge ProspectiveMerge) bool {
	if a.Validate() != nil || merge.Validate() != nil || proposal.Validate() != nil {
		return false
	}
	if proposal.Kind != EffectSourceIssueClosure || proposal.ClosureProposal == nil || !proposal.ClosureProposal.Resolves {
		return false
	}
	return a.ProposalDigest == proposal.Digest &&
		a.PublicationIdentity == merge.PublicationIdentity &&
		a.CandidateHeadSHA == merge.CandidateHeadSHA &&
		a.BaseRef == merge.BaseRef &&
		a.BaseSHA == merge.BaseSHA
}
