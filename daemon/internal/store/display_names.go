package store

import (
	"context"
	"errors"

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
		Task: domain.DisplayName{
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
	id, err := tx.subjectTask(ctx, projectID, subject)
	if err != nil {
		return nil, err
	}
	if id != nil {
		task, err := tx.GetTask(ctx, *id)
		if err != nil {
			return nil, err
		}
		names.Task = task.Name
	}
	if err := names.Validate(); err != nil {
		return nil, err
	}
	return names, nil
}
