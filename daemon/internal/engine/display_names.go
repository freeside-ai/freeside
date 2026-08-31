package engine

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func displayNames(
	ctx context.Context,
	st *store.Store,
	projectID domain.ProjectID,
	subject domain.Subject,
) (*domain.DisplayNames, error) {
	// Some pure constructor tests intentionally omit persistence. Keep their
	// presentation deterministic with the same identifier fallback the store
	// accessor uses; live workflows always provide a store and enrich it from
	// project and work-unit records.
	if st == nil {
		names := &domain.DisplayNames{
			Project: domain.DisplayName{
				Text: string(projectID), Source: domain.DisplayNameSourceIdentifier,
			},
			WorkUnit: domain.DisplayName{
				Text: string(subject.ID), Source: domain.DisplayNameSourceIdentifier,
			},
		}
		if subject.Type == domain.SubjectRun && subject.RunID != nil {
			names.WorkUnit.Text = string(*subject.RunID)
		}
		return names, names.Validate()
	}
	var names *domain.DisplayNames
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		names, err = tx.DisplayNamesFor(ctx, projectID, subject)
		return err
	})
	return names, err
}
