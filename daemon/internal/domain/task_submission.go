package domain

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrInvalidTaskSubmissionOrigin marks an unknown submission origin.
	ErrInvalidTaskSubmissionOrigin = errors.New("invalid task submission origin")
	// ErrTaskSubmissionOriginMismatch marks a submission whose device does not
	// match its origin: a device submission names one, a daemon one none.
	ErrTaskSubmissionOriginMismatch = errors.New("task submission device does not match its origin")
)

// TaskSubmissionOrigin names who submitted a task. The zero value "" is
// invalid by design; a body recorded before origins existed decodes as
// device (CanonicalizeStoredRow).
type TaskSubmissionOrigin string

const (
	// TaskSubmissionOriginDevice: a paired client submitted the task.
	TaskSubmissionOriginDevice TaskSubmissionOrigin = "device"
	// TaskSubmissionOriginDaemon: project-image upkeep submitted a
	// maintenance task for a repair-class refusal (plan §5.7, Project-image
	// upkeep). Its source is the refusal record, which the store requires to
	// exist on every write and decode.
	TaskSubmissionOriginDaemon TaskSubmissionOrigin = "daemon"
)

// AllTaskSubmissionOrigins lists every valid TaskSubmissionOrigin.
var AllTaskSubmissionOrigins = []TaskSubmissionOrigin{TaskSubmissionOriginDevice, TaskSubmissionOriginDaemon}

func (o TaskSubmissionOrigin) valid() bool {
	switch o {
	case TaskSubmissionOriginDevice, TaskSubmissionOriginDaemon:
		return true
	default:
		return false
	}
}

// ImageUpkeepSubmissionIdentity is the manual-submission identity of the one
// maintenance task project-image upkeep may open for a refusal record. Keying
// it on the record makes a second submission for the same refusal converge on
// the first.
func ImageUpkeepSubmissionIdentity(refusalID Digest) string {
	return "daemon:image-upkeep:" + string(refusalID)
}

// TaskSubmission is the durable, immutable record of one accepted submit_task
// command (plan §5.11 "A paired client may submit a task", §5.14). Unlike a
// decision Command it binds to no attention item: it names the task the command
// created or fetched by the project-scoped intake key and the specification run
// that task carries. It is write-once, keyed by CommandID (a client-generated
// idempotency key), so a retry under the same id returns the original record
// and a changed body under that id is a conflict; the CommandID is also unique
// across command kinds, so it never collides with a decision Command.
//
// Name is the task's stored display name at the time the command committed: the
// operator name when this command created the task, otherwise the name the
// first submission recorded. A fetch of an existing task therefore reports the
// name that first submission won, not the (ignored) name this command carried.
// The committed result revision is the store's as_of_revision for the record,
// so it is not carried in the body.
//
// Origin says who submitted it. A device submission names its DeviceID; a
// daemon one (project-image upkeep's maintenance task) names none, and its
// SourceDigest is the refusal record that caused it.
type TaskSubmission struct {
	CommandID          string               `json:"command_id"`
	Origin             TaskSubmissionOrigin `json:"origin"`
	DeviceID           *DeviceID            `json:"device_id"`
	ProjectID          ProjectID            `json:"project_id"`
	SourceDigest       Digest               `json:"source_digest"`
	TaskID             TaskID               `json:"task_id"`
	SpecificationRunID RunID                `json:"specification_run_id"`
	Name               DisplayName          `json:"name"`
}

// Validate reports whether the submission record is well-formed: identified by
// a command id, an origin with its matching device, project, task, and
// specification run; binding a
// sha256 source digest; and carrying a valid display name. It is the
// reconstruction backstop for records that did not pass through the accepting
// transaction (store decode, struct literals).
func (s TaskSubmission) Validate() error {
	if s.CommandID == "" {
		return fmt.Errorf("task submission command_id: %w", ErrEmptyID)
	}
	if !s.Origin.valid() {
		return fmt.Errorf("task submission %s origin %q: %w", s.CommandID, s.Origin, ErrInvalidTaskSubmissionOrigin)
	}
	if (s.Origin == TaskSubmissionOriginDevice) != (s.DeviceID != nil) {
		return fmt.Errorf("task submission %s origin %q: %w", s.CommandID, s.Origin, ErrTaskSubmissionOriginMismatch)
	}
	// The daemon: namespace is the daemon's own (ImageUpkeepSubmissionIdentity),
	// so a device cannot claim a command ID the daemon will submit under.
	if s.Origin == TaskSubmissionOriginDevice && strings.HasPrefix(s.CommandID, "daemon:") {
		return fmt.Errorf("task submission %s uses the daemon namespace: %w", s.CommandID, ErrTaskSubmissionOriginMismatch)
	}
	if s.DeviceID != nil && *s.DeviceID == "" {
		return fmt.Errorf("task submission %s device_id: %w", s.CommandID, ErrEmptyID)
	}
	if s.ProjectID == "" {
		return fmt.Errorf("task submission %s project_id: %w", s.CommandID, ErrEmptyID)
	}
	if s.TaskID == "" {
		return fmt.Errorf("task submission %s task_id: %w", s.CommandID, ErrEmptyID)
	}
	if s.SpecificationRunID == "" {
		return fmt.Errorf("task submission %s specification_run_id: %w", s.CommandID, ErrEmptyID)
	}
	if !isSHA256Digest(string(s.SourceDigest)) {
		return fmt.Errorf("task submission %s source_digest %q: %w", s.CommandID, s.SourceDigest, ErrInvalidDigest)
	}
	if err := s.Name.Validate(); err != nil {
		return fmt.Errorf("task submission %s name: %w", s.CommandID, err)
	}
	return nil
}

// CanonicalizeStoredRow reads a body recorded before origins existed as a
// device submission, which every such body was. Migration 0109 copied those
// bodies verbatim and set their origin column to device.
func (s *TaskSubmission) CanonicalizeStoredRow() {
	if s.Origin == "" {
		s.Origin = TaskSubmissionOriginDevice
	}
}
