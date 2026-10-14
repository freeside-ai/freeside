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

// A dispute settles every route that needs a person in this round. Accepted
// parking routes join it only when another answer requires a dispute;
// otherwise the existing finding-adjudication card retains its choices.
func (w *productionPublicationWorkflow) pendingAdjudicatedFindings(ctx context.Context, artifact domain.FindingAdjudication) ([]domain.FindingID, error) {
	var waiting, parked []domain.FindingID
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		policy, err := tx.GetResolvedPolicy(ctx, artifact.RunID)
		if err != nil {
			return err
		}
		threshold := resolvedFindingThreshold(policy, findingConfidenceThresholdKey)
		for _, entry := range artifact.Entries {
			accepted, err := entry.Accepted(threshold)
			if err != nil {
				return err
			}
			needsDecision := !accepted || entry.Route == domain.RouteDispute
			if entry.Route == domain.RouteDecline || entry.Route == domain.RouteDefer {
				finding, err := tx.GetFinding(ctx, entry.FindingID)
				if err != nil {
					return err
				}
				needsDecision = needsDecision || finding.Severity == domain.FindingSeverityP0 ||
					finding.Severity == domain.FindingSeverityP1 || (finding.External != nil && finding.Severity == "")
			}
			if needsDecision {
				waiting = append(waiting, entry.FindingID)
			} else if entry.Route.ParksRun() {
				parked = append(parked, entry.FindingID)
			}
		}
		return nil
	})
	if err != nil || len(waiting) == 0 {
		return nil, err
	}
	waiting = append(waiting, parked...)
	slices.Sort(waiting)
	return waiting, nil
}

func (w *productionPublicationWorkflow) awaitPendingFindingDecisions(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
	artifact domain.FindingAdjudication, routes map[domain.FindingID]domain.AdjudicationRoute,
	waiting []domain.FindingID,
) (productionReviewGateState, error) {
	if err := w.reserveFindingAdjudicationAttention(task, record); err != nil {
		return productionReviewPending, err
	}
	settled := maps.Clone(routes)
	for _, id := range waiting {
		delete(settled, id)
	}
	// Commit outcomes before the item. A retry uses the same batch and skips
	// existing outcomes; the human successor must not rewrite their provenance.
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return w.persistUnresolvedFindingRoutes(ctx, tx, task.ProjectID, artifact, settled, artifact.CreatedAt)
	}); err != nil {
		return productionReviewPending, err
	}
	reason := fmt.Sprintf("Adjudication requires a second decision. Choose a route for each listed finding (%d). Other findings already have routes; recorded declines and deferrals won't change. Remediation waits until you answer every listed finding.", len(waiting))
	reason, err := w.withExternalReviewFindings(ctx, task, reason)
	if err != nil {
		return productionReviewPending, err
	}
	if err := w.putReviewAttentionWithListedFindings(ctx, task, record, reason,
		domain.AttentionReviewDispute, productionReviewItemID(task.RunID, record.Round),
		[]domain.Action{domain.ActionAdjudicateFindings, domain.ActionDiscuss, domain.ActionStop}, nil, waiting); err != nil {
		return productionReviewPending, err
	}
	return productionReviewPending, nil
}

func (w *productionPublicationWorkflow) persistUnresolvedFindingRoutes(
	ctx context.Context, tx *store.WriteTx, projectID domain.ProjectID,
	artifact domain.FindingAdjudication, routes map[domain.FindingID]domain.AdjudicationRoute, at time.Time,
) error {
	unresolved := maps.Clone(routes)
	for id := range unresolved {
		finding, err := tx.GetFinding(ctx, id)
		if err != nil {
			return err
		}
		if finding.External != nil {
			_, err = tx.GetExternalFindingDisposition(ctx, id, artifact.Round)
		} else {
			_, err = tx.GetFindingDisposition(ctx, id, artifact.Round)
		}
		if err == nil {
			delete(unresolved, id)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if err := persistFindingRouteDispositions(ctx, tx, artifact, unresolved, at); err != nil {
		return err
	}
	return w.proposeFollowUpFilings(ctx, tx, projectID, artifact, unresolved, domain.FollowUpSourceDeferredDisposition)
}
