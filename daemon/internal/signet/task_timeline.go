package signet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TaskTimeline carries only recorded facts, plus the current source-labeled
// name claim. Every list is newest first, unlike RunTimeline.Milestones.
type TaskTimeline struct {
	AsOfRevision int64                 `json:"as_of_revision"`
	AsOf         time.Time             `json:"as_of"`
	TaskID       domain.TaskID         `json:"task_id"`
	ProjectID    domain.ProjectID      `json:"project_id"`
	Name         domain.DisplayName    `json:"name"`
	Events       []domain.TaskEvent    `json:"events"`
	Sections     []TaskTimelineSection `json:"sections"`
}

type TaskTimelineSection struct {
	CampaignID *domain.CampaignID `json:"campaign_id"`
	Events     []domain.TaskEvent `json:"events"`
	Runs       []TaskTimelineRun  `json:"runs"`
}

type TaskTimelineRun struct {
	RunID         domain.RunID               `json:"run_id"`
	Role          *domain.TaskRunRole        `json:"role"`
	AttemptNumber *int                       `json:"attempt_number"`
	AttemptReason *string                    `json:"attempt_reason"`
	ParentRunID   *domain.RunID              `json:"parent_run_id"`
	SupersededBy  *domain.RunID              `json:"superseded_by"`
	Milestones    []domain.RunMilestone      `json:"milestones"`
	Hold          *domain.RunHoldObservation `json:"hold"`
	Events        []domain.TaskEvent         `json:"events"`
}

type taskTimelineInput struct {
	run         domain.Run
	observation domain.RunObservation
	attempt     *domain.ProductionAttempt
	facts       runProjectionFacts
	prBinding   *domain.WorkUnitPRBinding
}

// GetTaskTimeline authenticates every run and its observations under one
// store revision, using the same trust gates as GetRunTimeline.
func (s *Service) GetTaskTimeline(ctx context.Context, id domain.TaskID) (TaskTimeline, error) {
	var out TaskTimeline
	taskLoaded := false
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		if err := validateServerState(state); err != nil {
			return err
		}
		snapshot, err := tx.GetTaskSnapshot(ctx, id)
		if err != nil {
			return err
		}
		taskLoaded = true
		if err := validateSnapshot(state, snapshot.Snapshot); err != nil {
			return err
		}
		task := snapshot.Value
		names, err := tx.DisplayNamesFor(ctx, task.ProjectID, domain.Subject{Type: domain.SubjectTask, ID: domain.SubjectID(id), TaskID: &id})
		if err != nil {
			return err
		}
		task.Name = names.Task
		ids, err := tx.TaskRunIDs(ctx, id)
		if err != nil {
			return err
		}
		// Enumerate from durable runs as well as the task index. Selecting
		// only indexed rows would hide a run whose reverse edge was lost.
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		runsByID := make(map[domain.RunID]store.Snapshotted[domain.Run], len(ids))
		for _, run := range runs {
			if run.Value.TaskID == id {
				runsByID[run.Value.ID] = run
			}
		}
		if len(runsByID) != len(ids) {
			return fmt.Errorf("task run membership: %w", ErrRunObservationIntegrity)
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		inputs := make([]taskTimelineInput, 0, len(ids))
		for _, runID := range ids {
			run, ok := runsByID[runID]
			if !ok {
				return fmt.Errorf("task run %s is unavailable: %w", runID, ErrRunObservationIntegrity)
			}
			if err := validateSnapshot(state, run.Snapshot); err != nil {
				return err
			}
			observation, err := tx.ObserveRun(ctx, runID)
			if err != nil {
				return err
			}
			if err := authenticateRunObservation(ctx, tx, state, run.Value, observation, items); err != nil {
				return asRunObservationIntegrityError(err)
			}
			observation = withAuthoritativeInvocationStatuses(observation)
			facts, err := runProjectionFactsFor(ctx, tx, run.Value, observation)
			if err != nil {
				return asRunObservationIntegrityError(err)
			}
			input := taskTimelineInput{run: run.Value, observation: observation, facts: facts}
			if run.Value.CampaignID != "" {
				attempt, err := tx.GetProductionAttempt(ctx, run.Value.CampaignID, run.Value.AttemptNumber)
				if err != nil {
					return err
				}
				// Initial approval and implementation submission commit together.
				// Retry reservations can legitimately precede their runs.
				if attempt.AttemptNumber == 1 && attempt.ApprovedSpecDigest != "" {
					if _, ok := runsByID[attempt.ImplementationRunID]; !ok {
						return fmt.Errorf("approved campaign %s lacks implementation run %s: %w",
							attempt.CampaignID, attempt.ImplementationRunID, ErrRunObservationIntegrity)
					}
				}
				input.attempt = &attempt
			}
			input.prBinding, err = taskTimelinePRBinding(ctx, tx, runID)
			if err != nil {
				return err
			}
			inputs = append(inputs, input)
		}
		out, err = taskTimeline(task, inputs, state.Revision, time.Now().UTC())
		return err
	})
	if err != nil {
		// After reconstructing the task, missing required history is corrupt
		// state rather than an absent requested resource.
		if taskLoaded && store.IsRowVerdict(err) {
			err = fmt.Errorf("task history: %w: %w", ErrRunObservationIntegrity, err)
		}
		return TaskTimeline{}, fmt.Errorf("get task %q timeline: %w", id, err)
	}
	return out, nil
}

func taskTimelinePRBinding(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (*domain.WorkUnitPRBinding, error) {
	declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		if store.IsRowVerdict(err) {
			return nil, fmt.Errorf("task run %s declaration: %w: %w", runID, ErrRunObservationIntegrity, err)
		}
		return nil, err
	}
	binding, err := tx.GetWorkUnitPRBinding(ctx, declaration.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		if store.IsRowVerdict(err) {
			return nil, fmt.Errorf("task run %s PR binding: %w: %w", runID, ErrRunObservationIntegrity, err)
		}
		return nil, err
	}
	// Re-anchor the PR to its published resource, as completion does. Keep
	// the original binding's instant through successor republication; the
	// event describes opening the PR, not its latest head update.
	if err := authenticatedCompletionPRBinding(ctx, tx, runID, declaration.ID); err != nil {
		return nil, asRunObservationIntegrityError(err)
	}
	return &binding, nil
}

func taskTimeline(task domain.Task, inputs []taskTimelineInput, revision int64, asOf time.Time) (TaskTimeline, error) {
	out := TaskTimeline{
		AsOfRevision: revision, AsOf: asOf, TaskID: task.ID, ProjectID: task.ProjectID, Name: task.Name,
		Events:   []domain.TaskEvent{{Kind: domain.TaskEventCreated, RecordedAt: task.CreatedAt}},
		Sections: []TaskTimelineSection{},
	}
	if err := orderTaskEvents(out.Events); err != nil {
		return TaskTimeline{}, err
	}
	groups := make(map[domain.CampaignID]*TaskTimelineSection)
	allocated := make(map[domain.CampaignID]time.Time)
	submitted := make(map[domain.RunID]time.Time)
	campaigns := []domain.CampaignID{}
	for _, input := range inputs {
		run := input.run
		if run.TaskID != task.ID || run.ProjectID != task.ProjectID {
			return TaskTimeline{}, ErrRunObservationIntegrity
		}
		at, observed := input.observation.SubmittedAt()
		submitted[run.ID] = at
		section := groups[run.CampaignID]
		if section == nil {
			section = &TaskTimelineSection{Events: []domain.TaskEvent{}, Runs: []TaskTimelineRun{}}
			if run.CampaignID != "" {
				section.CampaignID = &run.CampaignID
				campaigns = append(campaigns, run.CampaignID)
			}
			groups[run.CampaignID] = section
		}
		entry := TaskTimelineRun{
			RunID: run.ID, SupersededBy: input.facts.supersededBy, Hold: input.observation.Hold,
			Milestones: append([]domain.RunMilestone{}, input.observation.Milestones...), Events: []domain.TaskEvent{},
		}
		if run.CampaignID != "" {
			if !observed || input.attempt == nil {
				return TaskTimeline{}, fmt.Errorf("campaign run %s lacks submission or attempt: %w", run.ID, ErrRunObservationIntegrity)
			}
			role := domain.TaskRunImplementation
			if input.attempt.SpecificationRunID == run.ID {
				role = domain.TaskRunSpecification
			}
			entry.Role, entry.AttemptNumber = &role, &run.AttemptNumber
			if run.AttemptReason != "" {
				entry.AttemptReason = &run.AttemptReason
			}
			if run.ParentRunID != "" {
				entry.ParentRunID = &run.ParentRunID
			}
			if role == domain.TaskRunSpecification && run.AttemptNumber == 1 {
				allocated[run.CampaignID] = at
				section.Events = append(section.Events, domain.TaskEvent{
					Kind: domain.TaskEventCampaignAllocated, RecordedAt: at,
					CampaignID: &run.CampaignID, SpecificationRunID: &run.ID,
				})
			}
			// Retries inherit approval. Only the initial implementation's
			// submission is the transaction that first records that approval.
			if role == domain.TaskRunImplementation && run.AttemptNumber == 1 {
				section.Events = append(section.Events, domain.TaskEvent{
					Kind: domain.TaskEventSpecificationApproved, RecordedAt: at,
					CampaignID: &run.CampaignID, RunID: &run.ID,
					ApprovedSpecDigest: &input.attempt.ApprovedSpecDigest,
					SpecificationRunID: &input.attempt.SpecificationRunID,
				})
			}
		} else if at.After(allocated[""]) {
			allocated[""] = at
		}
		if binding := input.prBinding; binding != nil {
			entry.Events = append(entry.Events, domain.TaskEvent{
				Kind: domain.TaskEventPROpened, RecordedAt: binding.RecordedAt, RunID: &run.ID, PRNumber: &binding.PRNumber,
			})
		}
		if completion := input.facts.completion; completion != nil {
			entry.Events = append(entry.Events, domain.TaskEvent{
				Kind: domain.TaskEventPRMerged, RecordedAt: completion.RecordedAt, RunID: &run.ID,
				PRNumber: &completion.PRNumber, MergeCommitSHA: &completion.MergeCommitSHA,
			})
		}
		slices.SortStableFunc(entry.Milestones, func(a, b domain.RunMilestone) int { return b.RecordedAt.Compare(a.RecordedAt) })
		if err := orderTaskEvents(entry.Events); err != nil {
			return TaskTimeline{}, err
		}
		section.Runs = append(section.Runs, entry)
	}
	if !slices.Equal(campaigns, task.CampaignIDs) {
		return TaskTimeline{}, fmt.Errorf("task campaign membership: %w", ErrRunObservationIntegrity)
	}
	for campaignID, section := range groups {
		if campaignID != "" && allocated[campaignID].IsZero() {
			return TaskTimeline{}, fmt.Errorf("campaign %s lacks allocation evidence: %w", campaignID, ErrRunObservationIntegrity)
		}
		if err := orderTaskEvents(section.Events); err != nil {
			return TaskTimeline{}, err
		}
		slices.SortFunc(section.Runs, func(a, b TaskTimelineRun) int {
			return cmp.Or(submitted[b.RunID].Compare(submitted[a.RunID]), cmp.Compare(b.RunID, a.RunID))
		})
		out.Sections = append(out.Sections, *section)
	}
	slices.SortFunc(out.Sections, func(a, b TaskTimelineSection) int {
		var aID, bID domain.CampaignID
		if a.CampaignID != nil {
			aID = *a.CampaignID
		}
		if b.CampaignID != nil {
			bID = *b.CampaignID
		}
		return cmp.Or(allocated[bID].Compare(allocated[aID]), cmp.Compare(bID, aID))
	})
	return out, nil
}

func orderTaskEvents(events []domain.TaskEvent) error {
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("task event: %w: %w", ErrRunObservationIntegrity, err)
		}
	}
	slices.SortStableFunc(events, func(a, b domain.TaskEvent) int { return b.RecordedAt.Compare(a.RecordedAt) })
	return nil
}
