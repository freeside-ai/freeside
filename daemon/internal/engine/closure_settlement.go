package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RetireSettledClosureProposals supersedes the work unit's open closure cards
// once the observers have recorded a fact that settles them: its pull request
// merged or closed unmerged, or its bound issue closed. A card asks whether
// the merge should close the source issue, so a merge, a close, or an issue
// already closed leaves nothing to decide. It keys on the recorded facts, not
// on the task's completion, so it reaches a unit whose completion is
// foreclosed. A card is stamped at the settling fact's observed_at, never
// before its own creation. A unit with no PR binding or no settling fact
// retires nothing.
func RetireSettledClosureProposals(
	ctx context.Context, tx *store.WriteTx, declaration domain.WorkUnitDeclaration,
) ([]domain.ItemID, error) {
	reason, at, err := settledClosure(ctx, &tx.ReadTx, declaration)
	if err != nil || reason == "" {
		return nil, err
	}
	return tx.RetireOpenClosureProposals(ctx, declaration.RunID, reason, at)
}

// settledClosure returns the sentence and instant of the first fact that
// settles the unit's closure cards, or an empty reason while none does. Only
// the PR's identity is read from the binding, so the immutable first record
// serves; the effective binding would add successor reads that cannot change
// it.
func settledClosure(
	ctx context.Context, tx *store.ReadTx, declaration domain.WorkUnitDeclaration,
) (string, time.Time, error) {
	binding, err := tx.GetWorkUnitPRBinding(ctx, declaration.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	pull, err := tx.LatestPullMergeFact(ctx, binding.RepositoryID, binding.PRNumber)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return "", time.Time{}, err
	case pull.Merged:
		return fmt.Sprintf("Superseded because pull request #%d was merged; the merge settled whether it closes the source issue.", binding.PRNumber),
			pull.ObservedAt.UTC(), nil
	case pull.State == domain.PullRequestClosed:
		return fmt.Sprintf("Superseded because pull request #%d was closed without merging.", binding.PRNumber),
			pull.ObservedAt.UTC(), nil
	}
	if declaration.BoundIssue == nil {
		return "", time.Time{}, nil
	}
	issue, err := tx.LatestIssueStateFact(ctx, binding.RepositoryID, *declaration.BoundIssue)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return "", time.Time{}, err
	// Issue facts are keyed by issue, not unit: a closed fact observed before
	// this unit's PR existed belongs to an earlier unit bound to the same
	// issue, which may since have been reopened, so it settles nothing here.
	case issue.State == domain.IssueClosed && !issue.ObservedAt.Before(binding.RecordedAt):
		return fmt.Sprintf("Superseded because issue #%d is already closed.", issue.IssueNumber),
			issue.ObservedAt.UTC(), nil
	}
	return "", time.Time{}, nil
}

// reconcileSettledClosureProposals retires settled closure cards on every
// pass, so it converges after a crash and reaches cards opened before the
// sweep existed. The engine reconciles many times a second and a held card
// can wait hours, so candidates are settled in one read and only a unit with
// a settling fact costs a write. A unit whose declaration or facts fail their
// gates stays visible and is logged; the pass continues with the other units.
func (e *Engine) reconcileSettledClosureProposals(ctx context.Context) error {
	var declarations []domain.WorkUnitDeclaration
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		handles, err := tx.ListOpenClosureProposalSubjects(ctx)
		if err != nil {
			return err
		}
		for _, handle := range handles {
			declaration, err := tx.GetWorkUnitDeclaration(ctx, domain.WorkUnitID(handle))
			if err != nil {
				e.logger.Warn("settled closure card cleanup remains incomplete", "work_unit", handle, "error", err)
				continue
			}
			reason, _, err := settledClosure(ctx, tx, declaration)
			if err != nil {
				e.logger.Warn("settled closure card cleanup remains incomplete", "work_unit", handle, "error", err)
				continue
			}
			if reason != "" {
				declarations = append(declarations, declaration)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, declaration := range declarations {
		err := e.store.Write(ctx, func(tx *store.WriteTx) error {
			items, err := RetireSettledClosureProposals(ctx, tx, declaration)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return errReplay
			}
			return nil
		})
		if err != nil && !errors.Is(err, errReplay) {
			e.logger.Warn("settled closure card cleanup remains incomplete", "work_unit", declaration.ID, "error", err)
		}
	}
	return nil
}
