package domain

import "fmt"

// ProspectiveMergeIdentity names what a base-advance re-entry evaluates: the
// merge of the pull request's head into the advanced base, built locally and
// never pushed. MergeSHA is a merge commit whose parents are BaseSHA then
// HeadSHA, built with a fixed author, committer, timestamp, and message, so it
// is a function of the two parents and the merge result. HeadSHA stays the
// head the forge and its reviewers see.
//
// It is deliberately not ProspectiveMerge, the closure-approval binding: that
// type requires a publication identity a re-entry does not have until it
// publishes (devlog/2026-10-02-2255-prospective-merge-identity.md).
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
