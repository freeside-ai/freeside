package storetest

import (
	"context"
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// BindSubject supplies a minimal run parent for a run-scoped test item and
// binds its task before any decision-surface commitment is computed. Tests of
// run lineage should create their full run instead of using this helper.
func BindSubject(ctx context.Context, tx *store.WriteTx, item *domain.AttentionItem) error {
	var runID domain.RunID
	if item.Subject.RunID != nil {
		runID = *item.Subject.RunID
	} else if item.Subject.Type == domain.SubjectRun {
		runID = domain.RunID(item.Subject.ID)
	}
	if runID == "" {
		return nil
	}
	run, err := tx.GetRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		run = domain.Run{
			ID: runID, ProjectID: item.ProjectID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		}
		if err := tx.AssignTask(ctx, &run, nil); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	item.Subject.TaskID = &run.TaskID
	names, err := tx.DisplayNamesFor(ctx, item.ProjectID, item.Subject)
	if err != nil {
		return err
	}
	if item.DisplayNames != nil {
		names.Project = item.DisplayNames.Project
	}
	item.DisplayNames = names
	return nil
}
