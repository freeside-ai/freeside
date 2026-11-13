package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// RunPostCandidate names one follow-up filing or issue comment proposal
// instance whose subject is a run's work unit. It is a candidate only: the
// columns are copied keys, so a caller reads the instance and its ledger
// through the gated reads before showing anything about it.
type RunPostCandidate struct {
	InstanceID domain.ProposalInstanceID
	Kind       domain.EffectKind
}

// RunPostCandidates lists the follow-up filing and issue comment proposal
// instances whose subject is handle, in instance order. It reads the
// instance table directly rather than through cards, so a proposal that
// never had a card is listed too. It grants nothing; the run outcome list
// (issue #3149) is its only reader.
func (tx *ReadTx) RunPostCandidates(
	ctx context.Context, handle domain.OpaqueSubjectHandle,
) ([]RunPostCandidate, error) {
	fail := func(err error) ([]RunPostCandidate, error) {
		return nil, fmt.Errorf("run post candidates for subject %q: %w", handle, err)
	}
	rows, err := tx.tx.QueryContext(ctx, `
SELECT instance_id, effect_kind FROM effect_proposal_instances
WHERE subject_handle = ? AND effect_kind IN (?, ?)
ORDER BY instance_id`, handle, domain.EffectFollowUpFiling, domain.EffectIssueComment)
	if err != nil {
		return fail(err)
	}
	candidates := []RunPostCandidate{}
	for rows.Next() {
		var c RunPostCandidate
		if err := rows.Scan(&c.InstanceID, &c.Kind); err != nil {
			_ = rows.Close()
			return fail(err)
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fail(err)
	}
	return candidates, nil
}

// IssueCommentPolicyApproved reports whether an issue comment instance has a
// policy approval row, which stands in for its card when gates.issue_comment
// is off. It checks presence only and grants nothing: the run outcome list
// reads it to show an approved comment as waiting for its dispatcher, which
// re-gates the approval through ApprovedIssueComment before it posts.
func (tx *ReadTx) IssueCommentPolicyApproved(ctx context.Context, instanceID domain.ProposalInstanceID) (bool, error) {
	n, err := tx.countIssueCommentPolicyApprovals(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("policy approval for instance %q: %w", instanceID, err)
	}
	return n > 0, nil
}
