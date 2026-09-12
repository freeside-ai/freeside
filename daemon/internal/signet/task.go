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

type TaskSnapshot struct {
	AsOfRevision  int64 `json:"as_of_revision"`
	EntityVersion int64 `json:"entity_version"`
	Task          Task  `json:"task"`
}

// Task is the task's stored identity and name plus its ordered run history.
// Lifecycle describes the newest run for display; it never decides WIP.
type Task struct {
	ID              domain.TaskID        `json:"id"`
	ProjectID       domain.ProjectID     `json:"project_id"`
	DisplayNames    domain.DisplayNames  `json:"display_names"`
	Source          *TaskSource          `json:"source"`
	CreatedAt       time.Time            `json:"created_at"`
	LastActivityAt  time.Time            `json:"last_activity_at"`
	Lifecycle       *domain.RunLifecycle `json:"lifecycle"`
	CurrentPosition *TaskPosition        `json:"current_position"`
	CampaignIDs     []domain.CampaignID  `json:"campaign_ids"`
	RunIDs          []domain.RunID       `json:"run_ids"`
}

type TaskPosition struct {
	RunID      domain.RunID          `json:"run_id"`
	Stage      *string               `json:"stage"`
	Round      *int                  `json:"round"`
	HoldReason *domain.RunHoldReason `json:"hold_reason"`
}

// TaskSource renders both union arms explicitly, including their null fields.
// The stored domain union retains its existing persistence encoding.
type TaskSource struct {
	Kind               domain.SpecificationSourceKind `json:"kind"`
	WorkItemArtifactID *domain.ArtifactID             `json:"work_item_artifact_id"`
	IssueSubject       *domain.IssueSubjectRef        `json:"issue_subject"`
}

func projectTaskSource(source *domain.SpecificationSource) *TaskSource {
	if source == nil {
		return nil
	}
	out := &TaskSource{Kind: source.Kind, IssueSubject: source.IssueSubject}
	if source.WorkItemArtifactID != "" {
		out.WorkItemArtifactID = &source.WorkItemArtifactID
	}
	return out
}

func projectTaskSnapshot(ctx context.Context, tx *store.ReadTx, state store.ServerState, task domain.Task, runs map[domain.RunID]Run) (TaskSnapshot, error) {
	ids, err := tx.TaskRunIDs(ctx, task.ID)
	if err != nil {
		return TaskSnapshot{}, err
	}
	names, err := tx.DisplayNamesFor(ctx, task.ProjectID, domain.Subject{Type: domain.SubjectTask, ID: domain.SubjectID(task.ID), TaskID: &task.ID})
	if err != nil {
		return TaskSnapshot{}, err
	}
	value := Task{
		ID: task.ID, ProjectID: task.ProjectID, DisplayNames: *names, Source: projectTaskSource(task.Source),
		CreatedAt: task.CreatedAt, LastActivityAt: task.CreatedAt, CampaignIDs: task.CampaignIDs, RunIDs: ids,
	}
	campaigns := []domain.CampaignID{}
	for _, id := range ids {
		run, ok := runs[id]
		if !ok {
			return TaskSnapshot{}, fmt.Errorf("task %s run %s is unavailable: %w", task.ID, id, ErrRunObservationIntegrity)
		}
		if run.TaskID != task.ID || run.ProjectID != task.ProjectID {
			return TaskSnapshot{}, ErrRunObservationIntegrity
		}
		if run.CampaignID != nil && !slices.Contains(campaigns, *run.CampaignID) {
			campaigns = append(campaigns, *run.CampaignID)
		}
		if run.LastActivityAt != nil && run.LastActivityAt.After(value.LastActivityAt) {
			value.LastActivityAt = *run.LastActivityAt
		}
	}
	if !slices.Equal(campaigns, task.CampaignIDs) {
		return TaskSnapshot{}, ErrRunObservationIntegrity
	}
	if len(ids) > 0 {
		newest := runs[ids[len(ids)-1]]
		value.Lifecycle = &newest.Lifecycle
		position := &TaskPosition{RunID: newest.ID, HoldReason: newest.HoldReason}
		if len(newest.Stages) > 0 {
			position.Stage = &newest.Stages[len(newest.Stages)-1].Name
		}
		observation, err := tx.ObserveRun(ctx, newest.ID)
		if err != nil {
			return TaskSnapshot{}, err
		}
		review, err := runReviewFacts(ctx, tx, newest.ID, observation.Invocations)
		if err != nil {
			if errors.Is(err, store.ErrRowInconsistent) || errors.Is(err, domain.ErrParentKeyMismatch) {
				return TaskSnapshot{}, fmt.Errorf("task %s review position: %w: %w", task.ID, ErrRunObservationIntegrity, err)
			}
			return TaskSnapshot{}, err
		}
		if review != nil && len(review.Rounds) > 0 {
			position.Round = &review.Rounds[len(review.Rounds)-1].Round
		}
		value.CurrentPosition = position
	}
	// The summary depends on observations as well as the stored task. Use the
	// transaction's revision so a changed run pulse cannot reuse an old version.
	return TaskSnapshot{AsOfRevision: state.Revision, EntityVersion: state.Revision, Task: value}, nil
}
