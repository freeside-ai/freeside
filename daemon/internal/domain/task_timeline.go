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
	TaskEventCampaignAllocated     TaskEventKind = "campaign_allocated"
	TaskEventSpecificationApproved TaskEventKind = "specification_approved"
	TaskEventPROpened              TaskEventKind = "pr_opened"
	TaskEventPRMerged              TaskEventKind = "pr_merged"
)

var AllTaskEventKinds = []TaskEventKind{
	TaskEventCreated, TaskEventCampaignAllocated, TaskEventSpecificationApproved,
	TaskEventPROpened, TaskEventPRMerged,
}

func (k TaskEventKind) valid() bool {
	switch k {
	case TaskEventCreated, TaskEventCampaignAllocated, TaskEventSpecificationApproved,
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

// TaskEvent is a kind-scoped fact with explicit-null details. Campaign
// allocation names its specification run; approval also names the bound
// implementation run. PR events name their run, including legacy runs.
type TaskEvent struct {
	Kind               TaskEventKind `json:"kind"`
	RecordedAt         time.Time     `json:"recorded_at"`
	CampaignID         *CampaignID   `json:"campaign_id"`
	RunID              *RunID        `json:"run_id"`
	ApprovedSpecDigest *Digest       `json:"approved_spec_digest"`
	SpecificationRunID *RunID        `json:"specification_run_id"`
	PRNumber           *int          `json:"pr_number"`
	MergeCommitSHA     *string       `json:"merge_commit_sha"`
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
	switch e.Kind {
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
