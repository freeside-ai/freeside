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
    (command_id, device_id, project_id, task_id, specification_run_id, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, 1, ?, ?)`

// PutTaskSubmission write-once records one accepted submit_task command
// (domain.TaskSubmission). It is client-visible, so it runs inside Write, which
// stamps as_of_revision as the row's committed result: the revision an
// idempotent retry must return unchanged (§5.14 test 4). A byte-identical
// replay converges without a write; a changed body under the same command_id
// is a conflict. The command_id is unique across command kinds, so a decision
// command already recorded under this id is a cross-kind conflict.
func (tx *WriteTx) PutTaskSubmission(ctx context.Context, submission domain.TaskSubmission) error {
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
	if _, err := tx.tx.ExecContext(ctx, putTaskSubmissionSQL,
		submission.CommandID, submission.DeviceID, submission.ProjectID,
		submission.TaskID, submission.SpecificationRunID, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put task submission %q: %w", submission.CommandID, err)
	}
	return nil
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
		deviceID           string
		projectID          string
		taskID             string
		specificationRunID string
		snap               Snapshot
		body               []byte
	)
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT device_id, project_id, task_id, specification_run_id, entity_version, as_of_revision, body FROM task_submission_commands WHERE command_id = ?`, commandID).
		Scan(&deviceID, &projectID, &taskID, &specificationRunID, &snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
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
	if submission.CommandID != commandID ||
		submission.DeviceID != domain.DeviceID(deviceID) ||
		submission.ProjectID != domain.ProjectID(projectID) ||
		submission.TaskID != domain.TaskID(taskID) ||
		submission.SpecificationRunID != domain.RunID(specificationRunID) ||
		snap.EntityVersion != 1 || snap.AsOfRevision < 1 {
		return domain.TaskSubmission{}, Snapshot{}, fmt.Errorf("get task submission %q: %w", commandID, errRowInconsistent)
	}
	return submission, snap, nil
}
