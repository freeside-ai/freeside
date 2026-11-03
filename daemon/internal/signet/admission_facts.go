package signet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runAdmissionFacts reads what ran each admitted invocation of a run (#979):
// one fact set per execution admission and per review admission, in
// admission order. Every value is the admission record's; nothing is read
// from the current tree, identity, or enrollment. Execution admissions are
// read without re-applying current policy, as presentation must be, so a
// later floor or profile change cannot hide that an invocation ran.
//
// A review's independence compares its lineage group with the latest writer
// admission of each writing role admitted no later than the review, since
// that is the work the review could have judged. A writer whose admission
// recorded no lineage group, or a role with no admission yet, compares as
// unknown.
func runAdmissionFacts(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID,
) ([]domain.InvocationAdmissionFacts, error) {
	executions, err := tx.ListRunExecutionAdmissionRecords(ctx, runID)
	if err != nil {
		return nil, err
	}
	reviews, err := tx.ListRunReviewAdmissions(ctx, runID)
	if err != nil {
		return nil, err
	}
	facts := make([]domain.InvocationAdmissionFacts, 0, len(executions)+len(reviews))
	writers := make([]domain.InvocationAdmissionFacts, 0, len(executions))
	for _, admission := range executions {
		role, err := admittedWriterRole(ctx, tx, admission)
		if err != nil {
			return nil, fmt.Errorf("admission %q role: %w", admission.InvocationID, err)
		}
		admitted := domain.AdmittedFactsOfExecution(admission, role)
		facts = append(facts, admitted)
		if slices.Contains(domain.WritingRoles, role) {
			writers = append(writers, admitted)
		}
	}
	slices.SortFunc(writers, domain.CompareAdmissionFacts)
	for _, review := range reviews {
		facts = append(facts, domain.AdmittedFactsOfReview(review, writerLineageAt(writers, review.AdmittedAt)))
	}
	slices.SortFunc(facts, domain.CompareAdmissionFacts)
	return facts, nil
}

// writerLineageAt maps each writing role to the lineage group its latest
// admission no later than at recorded, empty when that admission recorded
// none. writers are writer facts in admission order.
func writerLineageAt(writers []domain.InvocationAdmissionFacts, at time.Time) map[domain.RoleName]string {
	lineage := make(map[domain.RoleName]string, len(domain.WritingRoles))
	for _, writer := range writers {
		if writer.AdmittedAt.After(at) {
			break
		}
		// A later writer of the role replaces an earlier one.
		lineage[*writer.Role] = ""
		if writer.LineageGroup != nil {
			lineage[*writer.Role] = *writer.LineageGroup
		}
	}
	return lineage
}

// admittedWriterRole is the ward role an execution admission ran as, read
// from its authenticated dispatch intent the way reviewRoundSubject reads a
// reviewed head's producer, never from the invocation's identifier. It maps
// the intent kinds as the engine's admission does (wardRole): a production
// invocation is the implementer's, a remediation round or an operator-feedback
// pass the remediator's, and a specification invocation or discussion the
// specifier's. A generic agent invocation runs in the feedback stage, which
// no ward role runs, and an admission with no retained intent (legacy) has
// no kind to read; both name none.
func admittedWriterRole(
	ctx context.Context, tx *store.ReadTx, admission domain.ExecutionAdmission,
) (domain.RoleName, error) {
	entry, err := tx.GetOutbox(ctx, string(admission.InvocationID))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := domain.AuthenticateInvocationDispatchIntent(domain.InvocationDispatchIntent{
		Kind: entry.Kind, IdempotencyKey: entry.IdempotencyKey, Payload: entry.Payload,
	}, admission.InvocationID, admission.RunID, admission.StageID); err != nil {
		return "", err
	}
	switch domain.InvocationIntentKind(entry.Kind) {
	case domain.ProductionInvocationRequestedKind:
		return domain.RoleImplementer, nil
	case domain.RemediationInvocationRequestedKind, domain.OperatorFeedbackInvocationRequestedKind:
		return domain.RoleRemediator, nil
	case domain.SpecificationInvocationRequestedKind, domain.SpecificationDiscussionRequestedKind:
		return domain.RoleSpecifier, nil
	case domain.AgentInvocationRequestedKind:
		return "", nil
	}
	return "", nil
}
