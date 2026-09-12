package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func (tx *ReadTx) gateRunTask(ctx context.Context, run domain.Run) error {
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if task.ProjectID != run.ProjectID || (run.CampaignID != "" && !slices.Contains(task.CampaignIDs, run.CampaignID)) {
		return errRowInconsistent
	}
	var taskID domain.TaskID
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id FROM task_runs WHERE run_id = ?`, run.ID).Scan(&taskID); err != nil {
		return err
	}
	if taskID != run.TaskID {
		return errRowInconsistent
	}
	expected, _, err := tx.campaignTask(ctx, run)
	if err != nil {
		return err
	}
	if expected != "" && expected != run.TaskID {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// campaignTask checks the task coordinates of the existing specification and
// retry parent without re-entering approval reconstruction, which reads items
// that in turn authenticate their subject's task. The full production policy
// gate still runs separately for PutRun and GetRun.
func (tx *ReadTx) campaignTask(ctx context.Context, run domain.Run) (domain.TaskID, domain.Digest, error) {
	if run.CampaignID == "" {
		return "", "", nil
	}
	attempt, err := tx.scanProductionAttempt(tx.tx.QueryRowContext(ctx,
		`SELECT `+productionAttemptColumns+` FROM production_attempts WHERE campaign_id = ? AND attempt_number = ?`, run.CampaignID, run.AttemptNumber))
	if err != nil {
		return "", "", notFoundOr(err)
	}
	if run.ID != attempt.SpecificationRunID && run.ID != attempt.ImplementationRunID {
		return "", "", domain.ErrParentKeyMismatch
	}
	var expected domain.TaskID
	for _, parentID := range []domain.RunID{attempt.ParentRunID, attempt.SpecificationRunID} {
		if parentID == "" || parentID == run.ID {
			continue
		}
		var projectID domain.ProjectID
		var taskID domain.TaskID
		var body []byte
		if err := tx.tx.QueryRowContext(ctx, `SELECT project_id, task_id, body FROM runs WHERE id = ?`, parentID).Scan(&projectID, &taskID, &body); err != nil {
			return "", "", notFoundOr(err)
		}
		parent, err := decode[domain.Run](body)
		if err != nil {
			return "", "", err
		}
		if parent.ID != parentID || parent.ProjectID != projectID || parent.TaskID != taskID || taskID == "" {
			return "", "", errRowInconsistent
		}
		if projectID != run.ProjectID || parent.CampaignID != run.CampaignID || (expected != "" && expected != taskID) {
			return "", "", domain.ErrParentKeyMismatch
		}
		expected = taskID
	}
	return expected, attempt.SourceDigest, nil
}

func (tx *ReadTx) subjectTask(ctx context.Context, projectID domain.ProjectID, subject domain.Subject) (*domain.TaskID, error) {
	if subject.Type == domain.SubjectTask {
		task, err := tx.GetTask(ctx, domain.TaskID(subject.ID))
		if err != nil {
			return nil, err
		}
		if task.ProjectID != projectID {
			return nil, domain.ErrParentKeyMismatch
		}
		return &task.ID, nil
	}
	if subject.Type == domain.SubjectProject || subject.Type == domain.SubjectSystem {
		return nil, nil
	}
	var runID domain.RunID
	if subject.RunID != nil {
		runID = *subject.RunID
	} else if subject.Type == domain.SubjectRun {
		runID = domain.RunID(subject.ID)
	}
	if runID == "" {
		return nil, nil
	}
	// This join checks task identity without re-entering production approval
	// reconstruction: that path itself reads run-scoped attention items.
	var body []byte
	var storedProject domain.ProjectID
	var storedTask domain.TaskID
	if err := tx.tx.QueryRowContext(ctx, `SELECT project_id, task_id, body FROM runs WHERE id = ?`, runID).Scan(&storedProject, &storedTask, &body); err != nil {
		return nil, notFoundOr(err)
	}
	run, err := decode[domain.Run](body)
	if err != nil {
		return nil, err
	}
	if run.ID != runID || run.ProjectID != storedProject || run.TaskID != storedTask || run.TaskID == "" {
		return nil, errRowInconsistent
	}
	if run.ProjectID != projectID {
		return nil, domain.ErrParentKeyMismatch
	}
	if err := tx.gateRunTask(ctx, run); err != nil {
		return nil, err
	}
	return &run.TaskID, nil
}

func (tx *ReadTx) bindSubjectTask(ctx context.Context, projectID domain.ProjectID, subject *domain.Subject) error {
	id, err := tx.subjectTask(ctx, projectID, *subject)
	if err != nil {
		return err
	}
	if subject.TaskID != nil && (id == nil || *subject.TaskID != *id) {
		return fmt.Errorf("subject task: %w", domain.ErrParentKeyMismatch)
	}
	subject.TaskID = id
	return nil
}

func (tx *ReadTx) gateSubjectTask(ctx context.Context, projectID domain.ProjectID, subject domain.Subject) error {
	want, err := tx.subjectTask(ctx, projectID, subject)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return errRowInconsistent
		}
		return err
	}
	if (want == nil) != (subject.TaskID == nil) || (want != nil && *want != *subject.TaskID) {
		return errRowInconsistent
	}
	return nil
}

func (tx *ReadTx) projectTaskName(ctx context.Context, item *domain.AttentionItem) error {
	if item.Subject.TaskID == nil {
		return nil
	}
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
