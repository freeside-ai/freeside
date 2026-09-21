package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

type TaskCancellationState string

const (
	TaskCancellationRequested TaskCancellationState = "requested"
	TaskCancellationConfirmed TaskCancellationState = "confirmed"
	TaskCancellationFailed    TaskCancellationState = "failed_to_stop"
)

var AllTaskCancellationStates = []TaskCancellationState{TaskCancellationRequested, TaskCancellationConfirmed, TaskCancellationFailed}

func (s TaskCancellationState) valid() bool {
	switch s {
	case TaskCancellationRequested, TaskCancellationConfirmed, TaskCancellationFailed:
		return true
	default:
		return false
	}
}

// StopTaskRequest is the complete decoded command. The observed version is the
// public task projection version, currently the pre-write server revision.
type StopTaskRequest struct {
	CommandID             string    `json:"command_id"`
	DeviceID              DeviceID  `json:"device_id"`
	TaskID                TaskID    `json:"task_id"`
	ProjectID             ProjectID `json:"project_id"`
	ExpectedSyncEpoch     string    `json:"expected_sync_epoch"`
	ExpectedEntityVersion int64     `json:"expected_entity_version"`
}

func (r StopTaskRequest) Validate() error {
	if r.CommandID == "" || r.DeviceID == "" || r.TaskID == "" || r.ProjectID == "" || r.ExpectedSyncEpoch == "" {
		return ErrEmptyID
	}
	if r.ExpectedEntityVersion < 1 {
		return ErrNonPositive
	}
	return nil
}

type TaskCancellationRun struct {
	RunID      RunID       `json:"run_id"`
	CampaignID *CampaignID `json:"campaign_id"`
}

// TaskCancellationTarget includes every owned run, including reserved work and
// older children. EpisodeOrdinal is zero for a task with no recorded start.
// The fence also forbids descendants; this list is not a newest-run selector.
type TaskCancellationTarget struct {
	TaskID         TaskID                `json:"task_id"`
	ProjectID      ProjectID             `json:"project_id"`
	EpisodeOrdinal int                   `json:"episode_ordinal"`
	Runs           []TaskCancellationRun `json:"runs"`
}

func (t TaskCancellationTarget) Validate() error {
	if t.TaskID == "" || t.ProjectID == "" {
		return ErrEmptyID
	}
	if t.EpisodeOrdinal < 0 || t.Runs == nil {
		return ErrEmptyField
	}
	seen := map[RunID]bool{}
	for _, r := range t.Runs {
		if r.RunID == "" || (r.CampaignID != nil && *r.CampaignID == "") {
			return ErrEmptyID
		}
		if seen[r.RunID] {
			return ErrDuplicate
		}
		seen[r.RunID] = true
	}
	return nil
}

// Digest pins the ordered authoritative membership, independent of mutable
// run status and task display fields.
func (t TaskCancellationTarget) Digest(syncEpoch string) Digest {
	fields := []string{"task-cancellation-target-v1", syncEpoch, string(t.TaskID), string(t.ProjectID), fmt.Sprint(t.EpisodeOrdinal), fmt.Sprint(len(t.Runs))}
	for _, run := range t.Runs {
		campaign := ""
		if run.CampaignID != nil {
			campaign = string(*run.CampaignID)
		}
		fields = append(fields, string(run.RunID), campaign)
	}
	var canonical strings.Builder
	for _, field := range fields {
		fmt.Fprintf(&canonical, "%d:%s", len(field), field)
	}
	return Digest(contentaddr.Sum([]byte(canonical.String())))
}

// TaskCancellationAcknowledgement is daemon-only evidence, never a client
// input. Confirmed asserts all owned executions are quiescent and launches are
// fenced. The runtime producer must reconcile ownership after restart before
// making that assertion. A failed acknowledgement leaves the fence in force.
type TaskCancellationAcknowledgement struct {
	ID             string                `json:"id"`
	RequestID      string                `json:"request_id"`
	TargetDigest   Digest                `json:"target_digest"`
	State          TaskCancellationState `json:"state"`
	EvidenceDigest Digest                `json:"evidence_digest"`
	RecordedAt     time.Time             `json:"recorded_at"`
}

func (a TaskCancellationAcknowledgement) Validate() error {
	if a.ID == "" || a.RequestID == "" {
		return ErrEmptyID
	}
	if a.State != TaskCancellationConfirmed && a.State != TaskCancellationFailed {
		return ErrImmutableTransition
	}
	if !isSHA256Digest(string(a.TargetDigest)) || !isSHA256Digest(string(a.EvidenceDigest)) {
		return ErrInvalidDigest
	}
	return cancellationTime(a.RecordedAt)
}

type TaskCancellation struct {
	RequestID       string                           `json:"request_id"`
	Target          TaskCancellationTarget           `json:"target"`
	TargetDigest    Digest                           `json:"target_digest"`
	SyncEpoch       string                           `json:"sync_epoch"`
	FenceRevision   int64                            `json:"fence_revision"`
	RequestedAt     time.Time                        `json:"requested_at"`
	State           TaskCancellationState            `json:"state"`
	Acknowledgement *TaskCancellationAcknowledgement `json:"acknowledgement"`
}

func (c TaskCancellation) Validate() error {
	if c.RequestID == "" || c.SyncEpoch == "" {
		return ErrEmptyID
	}
	if c.FenceRevision < 1 {
		return ErrNonPositive
	}
	if err := c.Target.Validate(); err != nil {
		return err
	}
	if c.TargetDigest != c.Target.Digest(c.SyncEpoch) {
		return ErrInvalidDigest
	}
	if err := cancellationTime(c.RequestedAt); err != nil {
		return err
	}
	if !c.State.valid() {
		return ErrImmutableTransition
	}
	if c.State == TaskCancellationRequested {
		if c.Acknowledgement != nil {
			return ErrImmutableTransition
		}
		return nil
	}
	a := c.Acknowledgement
	if a == nil {
		return ErrEmptyField
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if a.RequestID != c.RequestID || a.TargetDigest != c.TargetDigest || a.State != c.State || a.RecordedAt.Before(c.RequestedAt) {
		return ErrParentKeyMismatch
	}
	return nil
}

func cancellationTime(t time.Time) error {
	if t.IsZero() {
		return ErrMissingTimestamp
	}
	if t.Location() != time.UTC {
		return ErrTimestampNotUTC
	}
	return nil
}

// StopTaskReceipt freezes the cancellation snapshot seen at command acceptance.
// Sync carries subsequent acknowledgements; replay never rewrites this receipt.
type StopTaskReceipt struct {
	StopTaskRequest
	Cancellation TaskCancellation `json:"cancellation"`
}

func (r StopTaskReceipt) Validate() error {
	if err := r.StopTaskRequest.Validate(); err != nil {
		return err
	}
	if err := r.Cancellation.Validate(); err != nil {
		return err
	}
	// The client's ExpectedEntityVersion is any revision it could have observed
	// (1..the accepting revision minus 1), not exactly one below the fence: a
	// live task's revision advances between the client's read and the Stop. The
	// version-below-accepting-revision invariant is re-gated in the store against
	// the persisted as_of_revision, which this decoded type does not carry, so it
	// cannot be checked here. A shared fence's receipt may even carry a version
	// above FenceRevision. See devlog 2026-09-21 task-stop-live-revision.
	if r.TaskID != r.Cancellation.Target.TaskID || r.ProjectID != r.Cancellation.Target.ProjectID || r.ExpectedSyncEpoch != r.Cancellation.SyncEpoch {
		return ErrParentKeyMismatch
	}
	return nil
}
