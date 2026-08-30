package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// DisplayNamesFor derives the presentation labels shared by run projections
// and attention-item producers. Missing naming records fall back to stable
// identifiers, while other store failures stay loud.
func (tx *ReadTx) DisplayNamesFor(
	ctx context.Context,
	projectID domain.ProjectID,
	subject domain.Subject,
) (*domain.DisplayNames, error) {
	names := &domain.DisplayNames{
		Project: domain.DisplayName{
			Text: string(projectID), Source: domain.DisplayNameSourceIdentifier,
		},
		WorkUnit: domain.DisplayName{
			Text: string(subject.ID), Source: domain.DisplayNameSourceIdentifier,
		},
	}
	project, err := tx.GetProject(ctx, projectID)
	if err == nil {
		names.Project = domain.DisplayName{
			Text: project.Repo, Source: domain.DisplayNameSourceName,
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if subject.Type == domain.SubjectRun {
		runID := domain.RunID(subject.ID)
		if subject.RunID != nil {
			runID = *subject.RunID
		}
		names.WorkUnit.Text = string(runID)
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
		if err == nil && declaration.BoundIssue != nil {
			names.WorkUnit = domain.DisplayName{
				Text:   fmt.Sprintf("#%d", *declaration.BoundIssue),
				Source: domain.DisplayNameSourceName,
			}
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if err := names.Validate(); err != nil {
		return nil, err
	}
	return names, nil
}
