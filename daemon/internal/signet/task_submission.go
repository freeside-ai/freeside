package signet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TaskSubmitter performs the engine-side work of a submit_task command inside
// the accepting transaction: it creates the task for a deliberate
// submission and returns the task's identity, its specification run, and its stored
// display name. It is injected (WithTaskSubmitter) because the engine imports
// signet, so signet cannot call the engine directly. A project with no
// configured submission policy is reported as store.ErrNotFound, which the HTTP
// boundary answers 404 without enumerating projects.
type TaskSubmitter interface {
	SubmitTask(ctx context.Context, tx *store.WriteTx, in TaskSubmissionInput) (TaskSubmissionResult, error)
}

// TaskSubmissionInput is the submitter's input: the project, the submitted
// source and its digest (already stored in the blob store), and an optional
// operator-chosen name.
type TaskSubmissionInput struct {
	CommandID     string
	RequestDigest domain.Digest
	ProjectID     domain.ProjectID
	Source        []byte
	SourceDigest  domain.Digest
	OperatorName  string
}

// TaskSubmissionResult identifies the created task, its specification run,
// and its stored display name.
type TaskSubmissionResult struct {
	TaskID             domain.TaskID
	SpecificationRunID domain.RunID
	Name               domain.DisplayName
}

// submitTask accepts one submit_task ClientCommand and returns its committed
// result. Idempotency is by CommandID first (a retry returns the recorded
// result and starts no second run), then the device gate, then the injected
// submitter creates a task keyed by the command identity. The
// whole command commits in one transaction: the source and policy artifacts,
// the specification run, and the submission record.
func (s *Service) submitTask(ctx context.Context, in ClientCommand) (CommandResult, error) {
	p := in.SubmitTask
	if p.ProjectID == "" || p.Source == "" {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidSubmitTaskPayload)
	}
	// The envelope belongs to a decision command; a submit_task command carrying
	// it is malformed. The HTTP boundary already rejects it, but the service
	// fails closed too so no path accepts a bound-to-nothing command.
	if in.ExpectedEntityVersion != 0 {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrInvalidSubmitTaskPayload)
	}
	if s.blobs == nil || s.taskSubmitter == nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w", in.CommandID, ErrTaskSubmissionUnavailable)
	}
	source := []byte(p.Source)
	digest := domain.Digest(contentaddr.Sum(source))
	requestBytes, err := json.Marshal(struct {
		Device  domain.DeviceID
		Payload SubmitTaskPayload
	}{in.DeviceID, p})
	if err != nil {
		return CommandResult{}, err
	}
	requestDigest := domain.Digest(contentaddr.Sum(requestBytes))
	// Bytes land before the transaction so an artifact row never names a digest
	// the blob store cannot serve (the freesided submit / attachment pattern).
	// The put is content-addressed and idempotent; if the transaction below
	// rolls back (a replay, a refusal), the blob is a harmless orphan, never a
	// durable store effect, so the device gate's "before any write" still holds
	// for every committed row.
	if _, err := s.blobs.Put(digest, bytes.NewReader(source)); err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q store source: %w", in.CommandID, err)
	}
	var result CommandResult
	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		// Idempotency: a retried command_id returns the recorded result with no
		// second effect (§5.14 test 4). A command_id reused for a different
		// submission is an immutable conflict, not a silent replay: the recorded
		// device, project, source, and optional name must match this request.
		// Historical rows lack the requested-name fingerprint and retain their
		// original name-insensitive replay check. This mirrors the decision path,
		// where a changed body under an occupied command_id surfaces
		// store.ErrImmutableConflict from PutCommand (service.go); the fast replay
		// here returns before PutTaskSubmission would catch it, so the check lives
		// here. store.ErrImmutableConflict maps to 400 at the HTTP boundary.
		if recorded, snap, getErr := tx.GetTaskSubmissionSnapshot(ctx, in.CommandID); getErr == nil {
			original, err := tx.TaskSubmissionRequestDigest(ctx, in.CommandID)
			if err != nil {
				return err
			}
			if recorded.DeviceID != in.DeviceID || recorded.ProjectID != p.ProjectID ||
				recorded.SourceDigest != digest || (original != "" && original != requestDigest) {
				return fmt.Errorf("submit command %q: %w", in.CommandID, store.ErrImmutableConflict)
			}
			record := recorded
			result = CommandResult{Submission: &record, Revision: snap.AsOfRevision}
			return errReplay
		} else if !errors.Is(getErr, store.ErrNotFound) {
			return getErr
		}
		// A revoked (or unknown) device cannot submit; checked inside the
		// accepting transaction, before any write (§5.14 permanent test 15).
		if err := gateActiveDevice(ctx, tx, in.DeviceID); err != nil {
			return err
		}
		outcome, err := s.taskSubmitter.SubmitTask(ctx, tx, TaskSubmissionInput{
			CommandID: in.CommandID, RequestDigest: requestDigest,
			ProjectID: p.ProjectID, Source: source, SourceDigest: digest, OperatorName: p.Name,
		})
		if err != nil {
			return err
		}
		submission := domain.TaskSubmission{
			CommandID: in.CommandID, DeviceID: in.DeviceID, ProjectID: p.ProjectID,
			SourceDigest: digest, TaskID: outcome.TaskID, SpecificationRunID: outcome.SpecificationRunID,
			Name: outcome.Name,
		}
		if err := tx.PutTaskSubmissionRequest(ctx, submission, requestDigest); err != nil {
			return err
		}
		recorded, snap, err := tx.GetTaskSubmissionSnapshot(ctx, in.CommandID)
		if err != nil {
			return err
		}
		result = CommandResult{Submission: &recorded, Revision: snap.AsOfRevision}
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return CommandResult{}, err
	}
	return result, nil
}
