package domain

import "fmt"

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
type TaskSubmission struct {
	CommandID          string      `json:"command_id"`
	DeviceID           DeviceID    `json:"device_id"`
	ProjectID          ProjectID   `json:"project_id"`
	SourceDigest       Digest      `json:"source_digest"`
	TaskID             TaskID      `json:"task_id"`
	SpecificationRunID RunID       `json:"specification_run_id"`
	Name               DisplayName `json:"name"`
}

// Validate reports whether the submission record is well-formed: identified by
// a command id, device, project, task, and specification run; binding a
// sha256 source digest; and carrying a valid display name. It is the
// reconstruction backstop for records that did not pass through the accepting
// transaction (store decode, struct literals).
func (s TaskSubmission) Validate() error {
	if s.CommandID == "" {
		return fmt.Errorf("task submission command_id: %w", ErrEmptyID)
	}
	if s.DeviceID == "" {
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
