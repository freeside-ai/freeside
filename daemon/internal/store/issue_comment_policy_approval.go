package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Policy approval of an issue comment (issue #3150, plan §5.17). When
// gates.issue_comment resolves off in a run's resolved policy, the engine
// records this approval in place of the comment's effect_proposal card, and
// the issue comment dispatcher posts the comment. It is the policy actor's
// counterpart of a human decision, so an instance carries one recorder or the
// other, never both.
//
// What it re-checks: the writer re-gates the instance and its source as a
// human approve does (gateIssueCommentApproval), reads the gate from the
// run's resolved policy, and refuses when it is on, when a decision exists, or
// when the instance has a card. The reader (issueCommentPolicyApproval) re-derives
// the proposal digest and the policy digest from the instance and the run and
// re-reads the gate and the card. Every row it reads is write-once (the
// instance, the run's pinned policy, the card binding), so a comment the
// ledger posted stays readable; it never reads the trust profile or the
// Workflow layers, which change. The dispatcher rechecks the profile's
// issue_comment set at the default-branch tip before each post.

// ErrIssueCommentApprovalActorConflict is returned when an issue comment
// instance would carry approvals from both recorders: a human decision and a
// policy approval. The gate picks exactly one when the comment is proposed.
var ErrIssueCommentApprovalActorConflict = errors.New("issue comment approval already recorded by the other actor")

// ErrIssueCommentGateOn is returned when a policy approval is asked for a
// comment whose run's resolved policy turns gates.issue_comment on, or does
// not parse it: a person must decide that comment's card.
var ErrIssueCommentGateOn = errors.New("issue comment gate is on in the run's resolved policy")

// RecordPolicyIssueCommentApproval records the policy actor's approval of an
// issue comment instance. It re-gates the instance and refuses when its
// source no longer backs it, the run's resolved policy turns the gate on, a
// decision is recorded, or the instance has a card. A repeat for an instance
// already approved this way writes nothing.
func (tx *WriteTx) RecordPolicyIssueCommentApproval(
	ctx context.Context, instanceID domain.ProposalInstanceID, approvedAt time.Time,
) error {
	fail := func(err error) error {
		return fmt.Errorf("record policy issue comment approval %q: %w", instanceID, err)
	}
	if err := tx.RequireProposalWorkflowOpen(ctx, instanceID); err != nil {
		return fail(err)
	}
	if approvedAt.IsZero() {
		return fail(domain.ErrEmptyField)
	}
	instance, declaration, policy, err := tx.issueCommentPolicyBinding(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	// As a human approve does (gateIssueCommentApproval): a comment whose
	// source stopped backing it gains no authority.
	if err := tx.gateIssueCommentSourceCurrent(ctx, declaration, instance.Proposal.CommentProposal.Source); err != nil {
		return fail(err)
	}
	if domain.EffectGateOn(policy, domain.WorkflowGateIssueComment) {
		return fail(ErrIssueCommentGateOn)
	}
	decisions, err := tx.countProposalDecisions(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	cards, err := tx.countProposalItems(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	if decisions > 0 || cards > 0 {
		return fail(ErrIssueCommentApprovalActorConflict)
	}
	stored, found, err := tx.storedIssueCommentPolicyApproval(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	if found {
		if stored.proposalDigest != instance.Proposal.Digest || stored.policyDigest != policy.Digest {
			return fail(fmt.Errorf("a different binding is recorded: %w", errRowInconsistent))
		}
		return nil
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO issue_comment_policy_approvals
		(instance_id, proposal_digest, policy_digest, approved_at) VALUES (?, ?, ?, ?)`,
		instanceID, instance.Proposal.Digest, policy.Digest, formatTime(approvedAt.UTC())); err != nil {
		return fail(err)
	}
	return nil
}

// issueCommentPolicyBinding reads what a comment's policy approval binds: the
// instance, through the registry gate, and the work unit and resolved policy
// of the run its subject names.
func (tx *ReadTx) issueCommentPolicyBinding(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (domain.ProposalInstance, domain.WorkUnitDeclaration, domain.ResolvedPolicy, error) {
	fail := func(err error) (domain.ProposalInstance, domain.WorkUnitDeclaration, domain.ResolvedPolicy, error) {
		return domain.ProposalInstance{}, domain.WorkUnitDeclaration{}, domain.ResolvedPolicy{}, err
	}
	instance, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	comment := instance.Proposal.CommentProposal
	if instance.Proposal.Kind != domain.EffectIssueComment || comment == nil {
		return fail(fmt.Errorf("not an issue comment proposal: %w", domain.ErrEffectProposalInconsistent))
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, comment.SubjectHandle)
	if err != nil {
		return fail(err)
	}
	return instance, declaration, policy, nil
}

type issueCommentPolicyRow struct {
	proposalDigest domain.Digest
	policyDigest   domain.Digest
}

func (tx *ReadTx) storedIssueCommentPolicyApproval(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (issueCommentPolicyRow, bool, error) {
	var row issueCommentPolicyRow
	err := tx.tx.QueryRowContext(ctx, `SELECT proposal_digest, policy_digest
		FROM issue_comment_policy_approvals WHERE instance_id = ?`,
		instanceID).Scan(&row.proposalDigest, &row.policyDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return issueCommentPolicyRow{}, false, nil
	}
	if err != nil {
		return issueCommentPolicyRow{}, false, err
	}
	return row, true, nil
}

// issueCommentPolicyApproval checks a stored policy approval against the
// instance and the run's resolved policy it was read with: the digests must
// be the ones they derive, the gate must be off, and the instance must have
// no card. The writer refuses each of these, so a row that fails one is
// corruption, not a stale approval.
func (tx *ReadTx) issueCommentPolicyApproval(
	ctx context.Context, instance domain.ProposalInstance, policy domain.ResolvedPolicy,
) error {
	stored, found, err := tx.storedIssueCommentPolicyApproval(ctx, instance.ID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("policy approval row: %w", ErrNotFound)
	}
	if stored.proposalDigest != instance.Proposal.Digest {
		return fmt.Errorf("%w: policy approval digest disagrees with the current proposal", errRowInconsistent)
	}
	if stored.policyDigest != policy.Digest {
		return fmt.Errorf("%w: policy approval names another resolved policy", errRowInconsistent)
	}
	if domain.EffectGateOn(policy, domain.WorkflowGateIssueComment) {
		return fmt.Errorf("%w: policy approval under a resolved policy that turns the gate on", errRowInconsistent)
	}
	cards, err := tx.countProposalItems(ctx, instance.ID)
	if err != nil {
		return err
	}
	if cards > 0 {
		return fmt.Errorf("%w: policy approval on an instance with a card", errRowInconsistent)
	}
	return nil
}

func (tx *ReadTx) countIssueCommentPolicyApprovals(ctx context.Context, instanceID domain.ProposalInstanceID) (int, error) {
	var n int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issue_comment_policy_approvals WHERE instance_id = ?`, instanceID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (tx *ReadTx) countProposalItems(ctx context.Context, instanceID domain.ProposalInstanceID) (int, error) {
	var n int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM effect_proposal_items WHERE instance_id = ?`, instanceID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// PolicyApprovedIssueComment locates one policy-approved comment for the
// dispatcher's discovery. It is a pre-filter, not an authority: the
// dispatcher reads the approval through ApprovedIssueComment before it opens
// an intent or posts. ProjectID and ProposalBatchID are where the comment's
// notices go, the project and subject its card would have had. Err is set
// when the row's creation time or subject does not read; ProjectID and
// CreatedAt are then unset.
type PolicyApprovedIssueComment struct {
	InstanceID      domain.ProposalInstanceID
	ProposalBatchID domain.ProposalBatchID
	ProjectID       domain.ProjectID
	CreatedAt       time.Time
	Err             error
}

// ListPolicyApprovedIssueComments lists every comment instance with a policy
// approval, oldest instance first. A row that does not read is listed with
// its Err, so one bad row cannot hide the others.
func (tx *ReadTx) ListPolicyApprovedIssueComments(ctx context.Context) ([]PolicyApprovedIssueComment, error) {
	rows, err := tx.tx.QueryContext(ctx, `
SELECT a.instance_id, i.proposal_batch_id, i.subject_handle, i.created_at
FROM issue_comment_policy_approvals a
JOIN effect_proposal_instances i ON i.instance_id = a.instance_id
ORDER BY i.created_at, a.instance_id`)
	if err != nil {
		return nil, err
	}
	type listed struct {
		comment   PolicyApprovedIssueComment
		handle    domain.OpaqueSubjectHandle
		createdAt string
	}
	// Collect before resolving each subject, on the store's one connection.
	var found []listed
	for rows.Next() {
		var l listed
		if err := rows.Scan(&l.comment.InstanceID, &l.comment.ProposalBatchID, &l.handle, &l.createdAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		found = append(found, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	comments := make([]PolicyApprovedIssueComment, 0, len(found))
	for _, l := range found {
		createdAt, err := parseTime(l.createdAt)
		if err != nil {
			l.comment.Err = fmt.Errorf("%w: created_at: %w", errRowInconsistent, err)
			comments = append(comments, l.comment)
			continue
		}
		declaration, _, err := tx.ResolveProposalSubject(ctx, l.handle)
		if err != nil {
			l.comment.Err = fmt.Errorf("subject: %w", err)
			comments = append(comments, l.comment)
			continue
		}
		l.comment.CreatedAt, l.comment.ProjectID = createdAt, declaration.ProjectID
		comments = append(comments, l.comment)
	}
	return comments, nil
}
