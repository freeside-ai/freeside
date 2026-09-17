package signet

import (
	"cmp"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// taskHistoryEvents summarizes already authenticated records. It never derives
// an event from a current status, and does not create a second persistence log.
func taskHistoryEvents(task domain.Task, inputs []taskTimelineInput, sections []TaskTimelineSection) ([]domain.TaskEvent, error) {
	events := []domain.TaskEvent{{Kind: domain.TaskEventCreated, RecordedAt: task.CreatedAt}}
	for _, fact := range task.LifecycleFacts {
		var kind domain.TaskEventKind
		switch fact.Kind {
		case domain.TaskLifecycleStarted:
			kind = domain.TaskEventStarted
		case domain.TaskLifecycleCompleted:
			kind = domain.TaskEventCompleted
		case domain.TaskLifecycleAbandoned:
			kind = domain.TaskEventAbandoned
		}
		events = append(events, domain.TaskEvent{Kind: kind, RecordedAt: fact.RecordedAt, RunID: &fact.RunID, CampaignID: fact.CampaignID})
	}
	if cancellation := task.Cancellation; cancellation != nil {
		events = append(events, domain.TaskEvent{Kind: domain.TaskEventStopRequested, RecordedAt: cancellation.RequestedAt})
		if ack := cancellation.Acknowledgement; ack != nil {
			var kind domain.TaskEventKind
			switch ack.State {
			case domain.TaskCancellationConfirmed:
				kind = domain.TaskEventStopped
			case domain.TaskCancellationFailed:
				kind = domain.TaskEventStopFailed
			case domain.TaskCancellationRequested:
				return nil, ErrRunObservationIntegrity
			}
			events = append(events, domain.TaskEvent{Kind: kind, RecordedAt: ack.RecordedAt})
		}
	}
	for _, section := range sections {
		events = append(events, section.Events...)
		for _, run := range section.Runs {
			events = append(events, run.Events...)
		}
	}
	for _, input := range inputs {
		for _, milestone := range input.observation.Milestones {
			switch milestone.Kind {
			case domain.MilestoneRunSubmitted, domain.MilestoneInvocationStarted, domain.MilestoneTerminalRecorded,
				domain.MilestonePublicationReady, domain.MilestonePublicationBlocked:
				events = append(events, domain.TaskEvent{Kind: domain.TaskEventRunMilestone, RecordedAt: milestone.RecordedAt, RunID: &input.run.ID, Milestone: &milestone})
			case domain.MilestoneInvocationAdmitted, domain.MilestoneExecutionExportRecorded, domain.MilestoneExecutionOutcomeRecorded, domain.MilestoneWorkUnitCompleted:
				// The exact execution authority remains in run detail. Task and
				// PR completion already have their own summary events.
			}
		}
		if review := input.facts.review; review != nil {
			for _, round := range review.Rounds {
				detail := domain.TaskEventReview{InvocationID: round.InvocationID, Round: round.Round, HeadSHA: round.HeadSHA, BaseSHA: round.BaseSHA}
				if round.RequestedAt != nil {
					events = append(events, domain.TaskEvent{Kind: domain.TaskEventReviewRequested, RecordedAt: *round.RequestedAt, RunID: &input.run.ID, Review: &detail})
				}
				if round.CompletedAt != nil {
					terminal := detail
					kind := domain.TaskEventReviewCompleted
					terminal.Outcome = round.Outcome
					if round.Failure != nil {
						kind, terminal.Failure = domain.TaskEventReviewFailed, &round.Failure.Class
					}
					events = append(events, domain.TaskEvent{Kind: kind, RecordedAt: *round.CompletedAt, RunID: &input.run.ID, Review: &terminal})
				}
			}
		}
		for _, item := range input.readiness {
			// Legacy records can lack a timestamp or detailed verification.
			// Neither publication nor a later read supplies the missing fact.
			if item.CreatedAt == nil || item.ReadinessDetail == nil || item.Readiness == nil {
				continue
			}
			events = append(events, domain.TaskEvent{
				Kind: domain.TaskEventVerificationRecorded, RecordedAt: *item.CreatedAt, RunID: &input.run.ID,
				Verification: &domain.TaskEventVerification{ItemID: item.ID, Class: item.Readiness.Class, HeadSHA: item.ReadinessDetail.CandidateHead, BaseSHA: item.ReadinessDetail.Base.BaseSHA},
			})
		}
	}
	if err := orderTaskEvents(events); err != nil {
		return nil, err
	}
	// Equal timestamps do not establish causality. Break ties by stable source
	// identifiers so store enumeration order cannot reorder the display.
	slices.SortStableFunc(events, func(a, b domain.TaskEvent) int {
		return cmp.Or(b.RecordedAt.Compare(a.RecordedAt), cmp.Compare(a.Kind, b.Kind), cmp.Compare(historyEventSource(a), historyEventSource(b)))
	})
	return events, nil
}

func historyEventSource(event domain.TaskEvent) string {
	source := ""
	if event.CampaignID != nil {
		source += string(*event.CampaignID)
	}
	if event.RunID != nil {
		source += ":" + string(*event.RunID)
	}
	if event.Milestone != nil && event.Milestone.InvocationID != nil {
		source += ":" + string(*event.Milestone.InvocationID) + ":" + string(event.Milestone.Kind)
	}
	if event.Review != nil {
		source += ":" + string(event.Review.InvocationID)
	}
	if event.Verification != nil {
		source += ":" + string(event.Verification.ItemID)
	}
	return source
}
