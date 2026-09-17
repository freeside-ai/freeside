package domain

import (
	"fmt"
	"time"
)

// TaskEventKind names a recorded daemon fact, never inferred task history.
// The zero value is invalid.
type TaskEventKind string

const (
	TaskEventCreated               TaskEventKind = "task_created"
	TaskEventStarted               TaskEventKind = "task_started"
	TaskEventCompleted             TaskEventKind = "task_completed"
	TaskEventAbandoned             TaskEventKind = "task_abandoned"
	TaskEventStopRequested         TaskEventKind = "stop_requested"
	TaskEventStopped               TaskEventKind = "task_stopped"
	TaskEventStopFailed            TaskEventKind = "stop_failed"
	TaskEventRunMilestone          TaskEventKind = "run_milestone"
	TaskEventReviewRequested       TaskEventKind = "review_requested"
	TaskEventReviewCompleted       TaskEventKind = "review_completed"
	TaskEventReviewFailed          TaskEventKind = "review_failed"
	TaskEventVerificationRecorded  TaskEventKind = "verification_recorded"
	TaskEventCampaignAllocated     TaskEventKind = "campaign_allocated"
	TaskEventSpecificationApproved TaskEventKind = "specification_approved"
	TaskEventPROpened              TaskEventKind = "pr_opened"
	TaskEventPRMerged              TaskEventKind = "pr_merged"
)

var AllTaskEventKinds = []TaskEventKind{
	TaskEventCreated, TaskEventStarted, TaskEventCompleted, TaskEventAbandoned,
	TaskEventStopRequested, TaskEventStopped, TaskEventStopFailed, TaskEventRunMilestone,
	TaskEventReviewRequested, TaskEventReviewCompleted, TaskEventReviewFailed, TaskEventVerificationRecorded,
	TaskEventCampaignAllocated, TaskEventSpecificationApproved,
	TaskEventPROpened, TaskEventPRMerged,
}

func (k TaskEventKind) valid() bool {
	switch k {
	case TaskEventCreated, TaskEventStarted, TaskEventCompleted, TaskEventAbandoned,
		TaskEventStopRequested, TaskEventStopped, TaskEventStopFailed, TaskEventRunMilestone,
		TaskEventReviewRequested, TaskEventReviewCompleted, TaskEventReviewFailed, TaskEventVerificationRecorded,
		TaskEventCampaignAllocated, TaskEventSpecificationApproved,
		TaskEventPROpened, TaskEventPRMerged:
		return true
	default:
		return false
	}
}

// TaskRunRole describes a run's binding in its production attempt.
// Legacy runs without a campaign have no role; the zero value is invalid.
type TaskRunRole string

const (
	TaskRunSpecification  TaskRunRole = "specification"
	TaskRunImplementation TaskRunRole = "implementation"
)

var AllTaskRunRoles = []TaskRunRole{TaskRunSpecification, TaskRunImplementation}

func (r TaskRunRole) valid() bool {
	switch r {
	case TaskRunSpecification, TaskRunImplementation:
		return true
	default:
		return false
	}
}

// TaskEventReview pins a historical review to the invocation and candidate
// actually reviewed. The event kind distinguishes request, result, and failure.
type TaskEventReview struct {
	InvocationID InvocationID        `json:"invocation_id"`
	Round        int                 `json:"round"`
	HeadSHA      string              `json:"head_sha"`
	BaseSHA      string              `json:"base_sha"`
	Outcome      *ReviewOutcome      `json:"outcome"`
	Failure      *ReviewFailureClass `json:"failure"`
}

// TaskEventVerification references a recorded readiness decision, not the
// current readiness of its candidate. Its checklist remains on that decision.
type TaskEventVerification struct {
	ItemID  ItemID                `json:"item_id"`
	Class   ReadinessVerdictClass `json:"class"`
	HeadSHA string                `json:"head_sha"`
	BaseSHA string                `json:"base_sha"`
}

// TaskEvent is a kind-scoped fact with explicit-null details. Campaign
// allocation names its specification run; approval also names the bound
// implementation run. PR events name their run, including legacy runs.
type TaskEvent struct {
	Milestone          *RunMilestone          `json:"milestone"`
	Review             *TaskEventReview       `json:"review"`
	Verification       *TaskEventVerification `json:"verification"`
	Kind               TaskEventKind          `json:"kind"`
	RecordedAt         time.Time              `json:"recorded_at"`
	CampaignID         *CampaignID            `json:"campaign_id"`
	RunID              *RunID                 `json:"run_id"`
	ApprovedSpecDigest *Digest                `json:"approved_spec_digest"`
	SpecificationRunID *RunID                 `json:"specification_run_id"`
	PRNumber           *int                   `json:"pr_number"`
	MergeCommitSHA     *string                `json:"merge_commit_sha"`
}

func (e TaskEvent) Validate() error {
	if !e.Kind.valid() {
		return fmt.Errorf("task event kind %q: %w", e.Kind, ErrInvalidTaskEventKind)
	}
	if e.RecordedAt.IsZero() {
		return fmt.Errorf("task event recorded_at: %w", ErrMissingTimestamp)
	}
	if e.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("task event recorded_at: %w", ErrTimestampNotUTC)
	}
	if (e.CampaignID != nil && *e.CampaignID == "") || (e.RunID != nil && *e.RunID == "") ||
		(e.SpecificationRunID != nil && *e.SpecificationRunID == "") {
		return fmt.Errorf("task event identity: %w", ErrEmptyID)
	}
	if (e.ApprovedSpecDigest != nil && *e.ApprovedSpecDigest == "") ||
		(e.MergeCommitSHA != nil && *e.MergeCommitSHA == "") || (e.PRNumber != nil && *e.PRNumber < 1) {
		return fmt.Errorf("task event detail: %w", ErrTaskEventDetailMismatch)
	}
	check := func(campaign, run, digest, specification, pr, merge bool) error {
		if (e.CampaignID != nil) != campaign || (e.RunID != nil) != run ||
			(e.ApprovedSpecDigest != nil) != digest || (e.SpecificationRunID != nil) != specification ||
			(e.PRNumber != nil) != pr || (e.MergeCommitSHA != nil) != merge {
			return fmt.Errorf("task event %s: %w", e.Kind, ErrTaskEventDetailMismatch)
		}
		return nil
	}
	if (e.Milestone != nil) != (e.Kind == TaskEventRunMilestone) ||
		(e.Review != nil) != (e.Kind == TaskEventReviewRequested || e.Kind == TaskEventReviewCompleted || e.Kind == TaskEventReviewFailed) ||
		(e.Verification != nil) != (e.Kind == TaskEventVerificationRecorded) {
		return ErrTaskEventDetailMismatch
	}
	switch e.Kind {
	case TaskEventStarted, TaskEventCompleted, TaskEventAbandoned:
		return check(e.CampaignID != nil, true, false, false, false, false)
	case TaskEventStopRequested, TaskEventStopped, TaskEventStopFailed:
		return check(false, false, false, false, false, false)
	case TaskEventRunMilestone:
		if err := e.Milestone.Validate(); err != nil {
			return err
		}
		if e.RunID == nil || *e.RunID != e.Milestone.RunID || !e.RecordedAt.Equal(e.Milestone.RecordedAt) {
			return ErrTaskEventDetailMismatch
		}
		return check(false, true, false, false, false, false)
	case TaskEventReviewRequested, TaskEventReviewCompleted, TaskEventReviewFailed:
		r := e.Review
		if r.InvocationID == "" || r.Round < 1 || r.HeadSHA == "" || r.BaseSHA == "" ||
			(r.Outcome != nil) != (e.Kind == TaskEventReviewCompleted) || (r.Failure != nil) != (e.Kind == TaskEventReviewFailed) {
			return ErrTaskEventDetailMismatch
		}
		if r.Outcome != nil && !r.Outcome.valid() {
			return ErrTaskEventDetailMismatch
		}
		if r.Failure != nil && !r.Failure.valid() {
			return ErrTaskEventDetailMismatch
		}
		return check(false, true, false, false, false, false)
	case TaskEventVerificationRecorded:
		v := e.Verification
		if v.ItemID == "" || v.HeadSHA == "" || v.BaseSHA == "" || (v.Class != ReadinessReadyClean && v.Class != ReadinessReadyDegraded) {
			return ErrTaskEventDetailMismatch
		}
		return check(false, true, false, false, false, false)
	case TaskEventCreated:
		return check(false, false, false, false, false, false)
	case TaskEventCampaignAllocated:
		return check(true, false, false, true, false, false)
	case TaskEventSpecificationApproved:
		return check(true, true, true, true, false, false)
	case TaskEventPROpened:
		return check(false, true, false, false, true, false)
	case TaskEventPRMerged:
		return check(false, true, false, false, true, true)
	}
	return ErrInvalidTaskEventKind
}
