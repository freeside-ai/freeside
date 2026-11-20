package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// findingDecisionSplit divides a round that raises a dispute card. mustAnswer
// holds the findings a person must route: those domain.FindingNeedsPerson
// holds, and accepted parking routes, which join only when another answer
// already requires the card. settled holds the rest, which the card lists
// with their routes for the person to keep or override. A settled finding
// whose outcome is already recorded is in neither: a round an earlier build
// began recorded settled outcomes before raising its card, and an override
// could no longer change one, so the card leaves it out as it did then.
type findingDecisionSplit struct {
	mustAnswer, settled []domain.FindingID
}

// pendingAdjudicatedFindings returns the round's split, or a zero split when
// no finding needs a person and the existing finding-adjudication card keeps
// its choices.
func (w *productionPublicationWorkflow) pendingAdjudicatedFindings(ctx context.Context, artifact domain.FindingAdjudication) (findingDecisionSplit, error) {
	var waiting, parked, settled []domain.FindingID
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		policy, err := tx.GetResolvedPolicy(ctx, artifact.RunID)
		if err != nil {
			return err
		}
		threshold := resolvedFindingThreshold(policy, findingConfidenceThresholdKey)
		for _, entry := range artifact.Entries {
			finding, err := tx.GetFinding(ctx, entry.FindingID)
			if err != nil {
				return err
			}
			needsPerson, err := domain.FindingNeedsPerson(entry, finding, threshold)
			if err != nil {
				return err
			}
			switch {
			case needsPerson:
				waiting = append(waiting, entry.FindingID)
			case entry.Route.ParksRun():
				parked = append(parked, entry.FindingID)
			default:
				recorded, err := findingDispositionRecorded(ctx, tx, finding, artifact.Round)
				if err != nil {
					return err
				}
				if !recorded {
					settled = append(settled, entry.FindingID)
				}
			}
		}
		return nil
	})
	if err != nil || len(waiting) == 0 {
		return findingDecisionSplit{}, err
	}
	mustAnswer := append(waiting, parked...)
	slices.Sort(mustAnswer)
	slices.Sort(settled)
	return findingDecisionSplit{mustAnswer: mustAnswer, settled: settled}, nil
}

// awaitPendingFindingDecisions raises the dispute card listing every finding
// of the round. It records no route, follow-up proposal, or disposition, so
// nothing replies on GitHub while the card is open: the answered card's
// successor artifact records every route at once, keeping a settled finding's
// adjudicated entry unless the person overrode it.
func (w *productionPublicationWorkflow) awaitPendingFindingDecisions(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
	split findingDecisionSplit,
) (productionReviewGateState, error) {
	if err := w.reserveFindingAdjudicationAttention(task, record); err != nil {
		return productionReviewPending, err
	}
	reason := fmt.Sprintf("Adjudication requires a second decision. Choose a route for each finding that waits for you (%d).", len(split.mustAnswer))
	if len(split.settled) > 0 {
		reason += fmt.Sprintf(" The other findings (%d) already have a route; keep it or change it.", len(split.settled))
	}
	reason += " Nothing is recorded or replied on GitHub until you apply, and remediation waits until then."
	reason, err := w.withExternalReviewFindings(ctx, task, reason)
	if err != nil {
		return productionReviewPending, err
	}
	listed := slices.Concat(split.mustAnswer, split.settled)
	slices.Sort(listed)
	if err := w.putReviewAttentionWithListedFindings(ctx, task, record, reason,
		domain.AttentionReviewDispute, productionReviewItemID(task.RunID, record.Round),
		[]domain.Action{domain.ActionAdjudicateFindings, domain.ActionDiscuss, domain.ActionStop}, nil, listed); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPending, nil
}

func (w *productionPublicationWorkflow) persistUnresolvedFindingRoutes(
	ctx context.Context, tx *store.WriteTx, projectID domain.ProjectID,
	artifact domain.FindingAdjudication, routes map[domain.FindingID]domain.AdjudicationRoute, at time.Time,
	drafts followUpFilingDrafts,
) error {
	unresolved := maps.Clone(routes)
	for id := range unresolved {
		finding, err := tx.GetFinding(ctx, id)
		if err != nil {
			return err
		}
		recorded, err := findingDispositionRecorded(ctx, &tx.ReadTx, finding, artifact.Round)
		if err != nil {
			return err
		}
		if recorded {
			delete(unresolved, id)
		}
	}
	if err := persistFindingRouteDispositions(ctx, tx, artifact, unresolved, at); err != nil {
		return err
	}
	return w.proposeFollowUpFilings(ctx, tx, projectID, artifact, unresolved,
		domain.FollowUpSourceDeferredDisposition, drafts)
}

// findingDispositionRecorded reports whether a finding's outcome for the
// round is already stored, in the external or the internal outcome table.
func findingDispositionRecorded(ctx context.Context, tx *store.ReadTx, finding domain.Finding, round int) (bool, error) {
	var err error
	if finding.External != nil {
		_, err = tx.GetExternalFindingDisposition(ctx, finding.ID, round)
	} else {
		_, err = tx.GetFindingDisposition(ctx, finding.ID, round)
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
