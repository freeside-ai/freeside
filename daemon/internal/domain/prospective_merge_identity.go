package domain

import "fmt"

// ProspectiveMergeIdentity names what the handoff check verifies: the merge
// of the pull request's head into the target branch's tip, built locally and
// never pushed (engine/production_handoff.go). Review never sees it; since
// plan revision 110 review covers the head. MergeSHA is a merge commit whose
// parents are BaseSHA then HeadSHA, built with a fixed author, committer,
// timestamp, and message, so it is a function of the two parents and the
// merge result. HeadSHA stays the head the forge and its reviewers see.
//
// It is deliberately not ProspectiveMerge, the closure-approval binding: that
// type binds a publication identity this one does not carry
// (devlog/2026-10-02-2255-prospective-merge-identity.md).
type ProspectiveMergeIdentity struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	MergeSHA string `json:"merge_sha"`
}

// Validate reports whether the identity names three distinct commits. A merge
// equal to either parent is not a merge of them, and a head equal to the base
// leaves nothing to merge.
func (m ProspectiveMergeIdentity) Validate() error {
	if m.BaseSHA == "" || m.HeadSHA == "" || m.MergeSHA == "" {
		return fmt.Errorf("prospective merge identity: %w", ErrEmptyField)
	}
	if m.BaseSHA == m.HeadSHA || m.MergeSHA == m.BaseSHA || m.MergeSHA == m.HeadSHA {
		return fmt.Errorf("prospective merge identity repeats a commit: %w", ErrProspectiveMergeIdentityInconsistent)
	}
	return nil
}
