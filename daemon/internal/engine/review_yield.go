package engine

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (w *productionPublicationWorkflow) reviewYieldHistory(
	ctx context.Context, runID domain.RunID,
) (domain.ReviewYieldHistory, error) {
	var history domain.ReviewYieldHistory
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		history, err = tx.ReviewYieldHistory(ctx, runID)
		return err
	}); err != nil {
		return domain.ReviewYieldHistory{}, fmt.Errorf("load review yield history %q: %w", runID, err)
	}
	return history, nil
}

func deriveReviewYieldHistory(
	records []domain.ReviewRecord,
	dispositions []domain.ReviewDispositionRecord,
	findings map[domain.FindingID]domain.Finding,
) (domain.ReviewYieldHistory, error) {
	return store.DeriveReviewYieldHistory(records, dispositions, findings)
}
