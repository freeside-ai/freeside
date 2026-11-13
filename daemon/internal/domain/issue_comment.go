package domain

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// IssueCommentSource links a comment proposal back to what it came from: the
// finding, the FindingAdjudication artifact that routed it, and the content
// digest of the DuplicateSearch record whose answer names the target issue.
// The store re-reads that record by (AdjudicationDigest, FindingID) on every
// gate and requires its digest to equal SearchDigest; these fields only name
// which record to read.
type IssueCommentSource struct {
	FindingID          FindingID `json:"finding_id"`
	AdjudicationDigest Digest    `json:"adjudication_digest"`
	SearchDigest       Digest    `json:"search_digest"`
}

// Validate reports whether the source link is structurally sound.
func (s IssueCommentSource) Validate() error {
	if s.FindingID == "" {
		return fmt.Errorf("issue comment source finding_id: %w", ErrEmptyID)
	}
	if !contentaddr.Valid(string(s.AdjudicationDigest)) {
		return fmt.Errorf("issue comment source adjudication_digest %q: %w",
			s.AdjudicationDigest, ErrEffectProposalInconsistent)
	}
	if !contentaddr.Valid(string(s.SearchDigest)) {
		return fmt.Errorf("issue comment source search_digest %q: %w",
			s.SearchDigest, ErrEffectProposalInconsistent)
	}
	return nil
}

// IssueCommentParameters is the fixed parameter type for the issue_comment
// effect kind (plan §5.13). No field is authority: the store re-derives the
// repository from current rows, re-reads the duplicate search record the
// source names to re-derive the issue number, and screens the body again.
// There is deliberately no field that names who comments; the identity is
// chosen at dispatch.
type IssueCommentParameters struct {
	// SubjectHandle is the opaque work-unit handle, as the other kinds carry
	// it, so the store's subject_handle column and policy lookup apply.
	SubjectHandle OpaqueSubjectHandle `json:"subject_handle"`
	// Repository is the daemon's determination: the project's own repository.
	Repository FollowUpFilingRepository `json:"repository"`
	// IssueNumber is the issue the source's duplicate search named.
	IssueNumber int `json:"issue_number"`
	// Body is the comment text with its screening record; the producer puts
	// a fixed fallback here when the agent-influenced text is refused.
	Body   ScreenedIssueText  `json:"body"`
	Source IssueCommentSource `json:"source"`
}

// Validate reports whether the parameters are structurally sound. It is the
// reconstruction backstop: a decoded row cannot carry an unscreened body or a
// non-positive issue number.
func (p IssueCommentParameters) Validate() error {
	if p.SubjectHandle == "" {
		return fmt.Errorf("issue comment subject_handle: %w", ErrEmptyID)
	}
	if err := p.Repository.Validate(); err != nil {
		return err
	}
	if p.IssueNumber <= 0 {
		return fmt.Errorf("issue comment issue_number %d: %w", p.IssueNumber, ErrNonPositive)
	}
	if err := p.Body.validate("comment body", MaxFollowUpFilingBodyBytes, false); err != nil {
		return err
	}
	return p.Source.Validate()
}

// IssueCommentInput is the trusted construction input. The repository comes
// from the daemon's determination and the issue number from the duplicate
// search record alone: no field lets caller or agent text name either.
type IssueCommentInput struct {
	// SubjectHandle is the opaque work-unit handle.
	SubjectHandle OpaqueSubjectHandle
	// Repository is the project's own repository.
	Repository FollowUpFilingRepository
	// Search is the finding's duplicate search record; its DuplicateIssue is
	// the target and its key and digest are the source.
	Search DuplicateSearch
	// Body is the comment text with the record of its screening.
	Body ScreenedIssueText
}

// parameters derives the stored parameters from the trusted input. It rejects
// a search that answered "new", one recorded for another repository, and a
// body whose record is not passed under a registered ruleset.
func (in IssueCommentInput) parameters() (IssueCommentParameters, error) {
	if err := in.Repository.Validate(); err != nil {
		return IssueCommentParameters{}, err
	}
	if err := in.Search.Validate(); err != nil {
		return IssueCommentParameters{}, err
	}
	if in.Search.Repository.RepositoryID != in.Repository.RepositoryID {
		return IssueCommentParameters{}, fmt.Errorf("issue comment search repository: %w", ErrIssueCommentTargetMismatch)
	}
	issue, ok := in.Search.DuplicateIssue()
	if !ok {
		return IssueCommentParameters{}, ErrIssueCommentNotDuplicate
	}
	params := IssueCommentParameters{
		SubjectHandle: in.SubjectHandle, Repository: in.Repository, IssueNumber: issue, Body: in.Body,
		Source: IssueCommentSource{
			FindingID: in.Search.FindingID, AdjudicationDigest: in.Search.AdjudicationDigest,
			SearchDigest: in.Search.Digest,
		},
	}
	if err := params.Validate(); err != nil {
		return IssueCommentParameters{}, err
	}
	return params, nil
}

// GateIssueComment re-gates a comment proposal against the daemon's current
// repository determination and the duplicate search record the store re-read
// for the proposal's source. It refuses a repository that differs by id, a
// record that is not the one the source names (another key or another
// digest), a record that answered "new", and an issue number that is not the
// record's DuplicateIssue. The body screen is re-run by the store, which holds
// the screen this package cannot reach.
func GateIssueComment(proposal EffectProposal, repository FollowUpFilingRepository, search DuplicateSearch) error {
	if proposal.Kind != EffectIssueComment || proposal.CommentProposal == nil {
		return ErrEffectProposalInconsistent
	}
	comment := proposal.CommentProposal
	if err := comment.Validate(); err != nil {
		return err
	}
	if err := repository.Validate(); err != nil {
		return err
	}
	if comment.Repository.RepositoryID != repository.RepositoryID {
		return fmt.Errorf("issue comment repository: %w", ErrIssueCommentTargetMismatch)
	}
	if err := search.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrIssueCommentSourceMismatch, err)
	}
	if search.Digest != comment.Source.SearchDigest || search.FindingID != comment.Source.FindingID ||
		search.AdjudicationDigest != comment.Source.AdjudicationDigest {
		return fmt.Errorf("issue comment source names another search: %w", ErrIssueCommentSourceMismatch)
	}
	if search.Repository.RepositoryID != comment.Repository.RepositoryID {
		return fmt.Errorf("issue comment search repository: %w", ErrIssueCommentTargetMismatch)
	}
	issue, ok := search.DuplicateIssue()
	if !ok {
		return ErrIssueCommentNotDuplicate
	}
	if issue != comment.IssueNumber {
		return fmt.Errorf("issue comment issue %d, search names %d: %w",
			comment.IssueNumber, issue, ErrIssueCommentTargetMismatch)
	}
	return nil
}

// EffectGateOn reads a gates.<effect> switch from a run's resolved policy and
// reports whether a person must approve the effect. Absent means off, the
// key's default. Only the exact value "false", the one off value a Workflow
// layer accepts, turns it off; any other value turns the gate on (plan §3.2),
// so a configuration typo, or a looser spelling such as "0", never removes a
// person from the path. The engine reads it to choose between a card and a
// policy approval, and the store reads it again where a policy approval is
// written and read.
func EffectGateOn(policy ResolvedPolicy, key WorkflowPolicyKey) bool {
	for _, k := range policy.Keys {
		if k.Key == string(key) {
			return k.Value != "false"
		}
	}
	return false
}
