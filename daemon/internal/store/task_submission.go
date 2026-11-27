package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TaskSubmissionRequestDigest is absent only for historical command records,
// whose submitted optional name was never persisted.
func (tx *ReadTx) TaskSubmissionRequestDigest(ctx context.Context, commandID string) (domain.Digest, error) {
	var digest sql.NullString
	if err := tx.tx.QueryRowContext(ctx, `SELECT request_digest FROM task_submission_commands WHERE command_id = ?`, commandID).Scan(&digest); err != nil {
		return "", notFoundOr(err)
	}
	if digest.Valid && !contentaddr.Valid(digest.String) {
		return "", errRowInconsistent
	}
	return domain.Digest(digest.String), nil
}

// PutTaskSubmissionRequest records a new command's original decoded request
// without changing its public response body or historical replay semantics.
func (tx *WriteTx) PutTaskSubmissionRequest(ctx context.Context, submission domain.TaskSubmission, digest domain.Digest) error {
	if !contentaddr.Valid(string(digest)) {
		return domain.ErrInvalidDigest
	}
	if existing, err := tx.TaskSubmissionRequestDigest(ctx, submission.CommandID); err == nil {
		if existing != digest {
			return ErrImmutableConflict
		}
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := tx.PutTaskSubmission(ctx, submission); err != nil {
		return err
	}
	_, err := tx.tx.ExecContext(ctx, `UPDATE task_submission_commands SET request_digest = ? WHERE command_id = ? AND request_digest IS NULL`, digest, submission.CommandID)
	return err
}

const putTaskSubmissionSQL = `
INSERT INTO task_submission_commands
    (command_id, origin, device_id, project_id, task_id, specification_run_id, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`

// ErrDaemonSubmissionUnsourced is returned (wrapped) when a daemon-origin
// task submission does not name the source its kind requires: a recorded
// repair-class image upkeep refusal for its project's repository, or an
// agent_base_bump schedule occurrence and the bump task's fixed text.
var ErrDaemonSubmissionUnsourced = errors.New("daemon-origin task submission names no source its kind accepts")

// gateDaemonSubmissionSource is the trust gate for a daemon-origin
// submission, run on every write and decode. The daemon submits two kinds
// of task (plan §5.7), told apart by the command ID's namespace:
//
//   - Project-image upkeep's maintenance task, only for a refusal that
//     needs a repository repair, so the refusal record its SourceDigest
//     names must exist, be a repair, and name the repository of the
//     submission's project.
//   - The agent-base bump task, whose command ID names the schedule
//     occurrence that opened it. The occurrence must be recorded for an
//     agent_base_bump schedule. On write, SourceDigest must also be the
//     digest of the task text this build fixes, so the task asks nothing
//     else; decode leaves that check out, because a row keeps the text of
//     the build that wrote it and a later build may word it differently.
//
// Any other daemon namespace is refused. The origin, command ID, and source
// are body and column values, never trusted on their own. A device
// submission passes through.
func (tx *ReadTx) gateDaemonSubmissionSource(ctx context.Context, submission domain.TaskSubmission) error {
	switch submission.Origin {
	case domain.TaskSubmissionOriginDevice:
		return nil
	case domain.TaskSubmissionOriginDaemon:
		if _, _, _, ok := domain.ParseAgentBaseBumpSubmissionIdentity(submission.CommandID); ok {
			return tx.gateAgentBaseBumpSource(ctx, submission)
		}
		if !domain.IsImageUpkeepSubmission(submission.CommandID) {
			return fmt.Errorf("command %q: %w", submission.CommandID, ErrDaemonSubmissionUnsourced)
		}
		refusal, err := tx.GetImageUpkeepRefusal(ctx, submission.SourceDigest)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("source %q: %w", submission.SourceDigest, ErrDaemonSubmissionUnsourced)
		}
		if err != nil {
			return err
		}
		project, err := tx.GetProject(ctx, submission.ProjectID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("project %q: %w", submission.ProjectID, ErrDaemonSubmissionUnsourced)
		}
		if err != nil {
			return err
		}
		if refusal.Class != domain.ImageUpkeepRefusalRepair || refusal.RepositoryID != project.RepositoryID {
			return fmt.Errorf("source %q class %q repository %d: %w",
				submission.SourceDigest, refusal.Class, refusal.RepositoryID, ErrDaemonSubmissionUnsourced)
		}
		return nil
	}
	return fmt.Errorf("origin %q: %w", submission.Origin, domain.ErrInvalidTaskSubmissionOrigin)
}

// gateAgentBaseBumpSource is gateDaemonSubmissionSource's check of a bump
// submission.
func (tx *ReadTx) gateAgentBaseBumpSource(ctx context.Context, submission domain.TaskSubmission) error {
	scheduleID, generation, nominal, _ := domain.ParseAgentBaseBumpSubmissionIdentity(submission.CommandID)
	if _, err := tx.GetScheduleOccurrence(ctx, scheduleID, generation, nominal); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("bump occurrence %s/%d: %w", scheduleID, generation, ErrDaemonSubmissionUnsourced)
		}
		return err
	}
	schedule, _, err := tx.GetScheduleSnapshot(ctx, scheduleID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("bump schedule %s: %w", scheduleID, ErrDaemonSubmissionUnsourced)
	}
	if err != nil {
		return err
	}
	if schedule.Kind != domain.ScheduleAgentBaseBump {
		return fmt.Errorf("bump schedule %s of kind %q: %w", scheduleID, schedule.Kind, ErrDaemonSubmissionUnsourced)
	}
	return nil
}

// PutTaskSubmission write-once records one accepted submit_task command
// (domain.TaskSubmission). It is client-visible, so it runs inside Write, which
// stamps as_of_revision as the row's committed result: the revision an
// idempotent retry must return unchanged (§5.14 test 4). A byte-identical
// replay converges without a write; a changed body under the same command_id
// is a conflict. The command_id is unique across command kinds, so a decision
// command already recorded under this id is a cross-kind conflict.
func (tx *WriteTx) PutTaskSubmission(ctx context.Context, submission domain.TaskSubmission) error {
	if err := tx.rejectStopCommandID(ctx, submission.CommandID); err != nil {
		return err
	}
	if err := tx.rejectRecordedCommandID(ctx, submission.CommandID); err != nil {
		return err
	}
	if err := submission.Validate(); err != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, err)
	}
	body, err := encode(submission)
	if err != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, err)
	}
	existing, err := tx.existingBody(ctx,
		`SELECT body FROM task_submission_commands WHERE command_id = ?`, submission.CommandID)
	if err != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, err)
	}
	if existing != nil {
		if string(existing) == body {
			return nil
		}
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, ErrImmutableConflict)
	}
	// The command_id is unique across command kinds: a decision command already
	// recorded under this id makes this submission an immutable conflict.
	if conflict, err := tx.existingBody(ctx,
		`SELECT body FROM commands WHERE command_id = ?`, submission.CommandID); err != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, err)
	} else if conflict != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, ErrImmutableConflict)
	}
	if err := tx.gateDaemonSubmissionSource(ctx, submission); err != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, err)
	}
	if _, _, _, bump := domain.ParseAgentBaseBumpSubmissionIdentity(submission.CommandID); bump &&
		submission.Origin == domain.TaskSubmissionOriginDaemon &&
		submission.SourceDigest != domain.AgentBaseBumpSourceDigest() {
		return fmt.Errorf("put task submission %q: bump source %q: %w",
			submission.CommandID, submission.SourceDigest, ErrDaemonSubmissionUnsourced)
	}
	if _, err := tx.tx.ExecContext(ctx, putTaskSubmissionSQL,
		submission.CommandID, submission.Origin, submission.DeviceID, submission.ProjectID,
		submission.TaskID, submission.SpecificationRunID, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, err)
	}
	return nil
}

// ListAgentBaseBumpSubmissions returns the project's agent-base bump
// submissions (domain.AgentBaseBumpSubmissionIdentity) in command-ID order,
// each read through GetTaskSubmission, so each is gated as any read is.
func (tx *ReadTx) ListAgentBaseBumpSubmissions(
	ctx context.Context, projectID domain.ProjectID,
) ([]domain.TaskSubmission, error) {
	rows, err := tx.tx.QueryContext(ctx, `
SELECT command_id FROM task_submission_commands
WHERE origin = ? AND project_id = ? AND substr(command_id, 1, ?) = ?
ORDER BY command_id`,
		domain.TaskSubmissionOriginDaemon, projectID,
		len(domain.AgentBaseBumpSubmissionPrefix), domain.AgentBaseBumpSubmissionPrefix)
	if err != nil {
		return nil, fmt.Errorf("list agent base bump submissions: %w", err)
	}
	var commandIDs []string
	for rows.Next() {
		var commandID string
		if err := rows.Scan(&commandID); err != nil {
			_ = rows.Close() // the scan error is the one returned
			return nil, fmt.Errorf("list agent base bump submissions: %w", err)
		}
		commandIDs = append(commandIDs, commandID)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list agent base bump submissions: %w", err)
	}
	submissions := make([]domain.TaskSubmission, 0, len(commandIDs))
	for _, commandID := range commandIDs {
		submission, err := tx.GetTaskSubmission(ctx, commandID)
		if err != nil {
			return nil, err
		}
		submissions = append(submissions, submission)
	}
	return submissions, nil
}

// GetTaskSubmission returns the recorded submission for a command id.
func (tx *ReadTx) GetTaskSubmission(ctx context.Context, commandID string) (domain.TaskSubmission, error) {
	submission, _, err := tx.GetTaskSubmissionSnapshot(ctx, commandID)
	return submission, err
}

// GetTaskSubmissionSnapshot returns the submission together with its persisted
// sync metadata. The row is write-once, so its AsOfRevision is the command's
// original committed result. Every extracted column is cross-checked against
// the body, so a forged or corrupt row fails loudly instead of returning a
// record its columns do not back.
func (tx *ReadTx) GetTaskSubmissionSnapshot(
	ctx context.Context, commandID string,
) (domain.TaskSubmission, Snapshot, error) {
	var (
		origin             string
		deviceID           sql.NullString
		projectID          string
		taskID             string
		specificationRunID string
		snap               Snapshot
		body               []byte
	)
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT origin, device_id, project_id, task_id, specification_run_id, entity_version, as_of_revision, body FROM task_submission_commands WHERE command_id = ?`, commandID).
		Scan(&origin, &deviceID, &projectID, &taskID, &specificationRunID, &snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
		return domain.TaskSubmission{}, Snapshot{}, fmt.Errorf("get task submission %q: %w", commandID, notFoundOr(err))
	}
	submission, err := decode[domain.TaskSubmission](body)
	if err != nil {
		return domain.TaskSubmission{}, Snapshot{}, fmt.Errorf("get task submission %q: %w", commandID, err)
	}
	// Re-run the domain validation at this reconstruction boundary so a
	// tampered body (its source digest or name are body-only, not columns)
	// fails closed instead of returning a record the columns cannot back.
	if err := submission.Validate(); err != nil {
		return domain.TaskSubmission{}, Snapshot{}, fmt.Errorf("get task submission %q: %w", commandID, errRowInconsistent)
	}
	var bodyDeviceID string
	if submission.DeviceID != nil {
		bodyDeviceID = string(*submission.DeviceID)
	}
	if submission.CommandID != commandID ||
		string(submission.Origin) != origin ||
		!optionalStringEqual(deviceID, bodyDeviceID) ||
		submission.ProjectID != domain.ProjectID(projectID) ||
		submission.TaskID != domain.TaskID(taskID) ||
		submission.SpecificationRunID != domain.RunID(specificationRunID) ||
		snap.EntityVersion != 1 || snap.AsOfRevision < 1 {
		return domain.TaskSubmission{}, Snapshot{}, fmt.Errorf("get task submission %q: %w", commandID, errRowInconsistent)
	}
	// A stored daemon-origin row whose refusal is missing or does not back it
	// is an inconsistent row, not an absent one.
	if err := tx.gateDaemonSubmissionSource(ctx, submission); errors.Is(err, ErrDaemonSubmissionUnsourced) {
		return domain.TaskSubmission{}, Snapshot{}, fmt.Errorf("get task submission %q: %w",
			commandID, errors.Join(err, errRowInconsistent))
	} else if err != nil {
		return domain.TaskSubmission{}, Snapshot{}, fmt.Errorf("get task submission %q: %w", commandID, err)
	}
	return submission, snap, nil
}
