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

// TaskSubmissionInput is the submitter's input: who submitted it, the
// project, the submitted source and its digest (already stored in the blob
// store), an optional operator-chosen name, and the operator's task lines.
type TaskSubmissionInput struct {
	CommandID string
	// Origin selects the task's manual-submission identity: a device
	// command's is namespaced under client:, and a daemon one's is its
	// command ID, already in the daemon: namespace.
	Origin        domain.TaskSubmissionOrigin
	RequestDigest domain.Digest
	ProjectID     domain.ProjectID
	Source        []byte
	SourceDigest  domain.Digest
	OperatorName  string
	// TaskLines are validated and in canonical role order; the submitter
	// records one line per entry on the task it creates.
	TaskLines []domain.TaskLineChoice
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
	// The lines are checked and put in canonical order before the request
	// digest is taken, so a malformed set writes nothing and a retry that
	// lists the same lines in another order is the same request.
	lines, err := canonicalTaskLines(p.TaskLines)
	if err != nil {
		return CommandResult{}, fmt.Errorf("submit command %q: %w: %w", in.CommandID, ErrInvalidSubmitTaskPayload, err)
	}
	p.TaskLines = lines
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
		// device, project, source, optional name, and task lines must match
		// this request.
		// Historical rows lack the requested-name fingerprint and retain their
		// original name-insensitive replay check. Such a row predates task lines,
		// so a retry that carries any is a different request, not a replay that
		// would return success and record none. This mirrors the decision path,
		// where a changed body under an occupied command_id surfaces
		// store.ErrImmutableConflict from PutCommand (service.go); the fast replay
		// here returns before PutTaskSubmission would catch it, so the check lives
		// here. store.ErrImmutableConflict maps to 400 at the HTTP boundary.
		if recorded, snap, getErr := tx.GetTaskSubmissionSnapshot(ctx, in.CommandID); getErr == nil {
			original, err := tx.TaskSubmissionRequestDigest(ctx, in.CommandID)
			if err != nil {
				return err
			}
			// A daemon-origin record names no device, so no device's retry
			// can replay it.
			if recorded.Origin != domain.TaskSubmissionOriginDevice || recorded.DeviceID == nil ||
				*recorded.DeviceID != in.DeviceID || recorded.ProjectID != p.ProjectID ||
				recorded.SourceDigest != digest || (original != "" && original != requestDigest) ||
				(original == "" && len(p.TaskLines) != 0) {
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
			CommandID: in.CommandID, Origin: domain.TaskSubmissionOriginDevice, RequestDigest: requestDigest,
			ProjectID: p.ProjectID, Source: source, SourceDigest: digest, OperatorName: p.Name,
			TaskLines: p.TaskLines,
		})
		if err != nil {
			return err
		}
		submission := domain.TaskSubmission{
			CommandID: in.CommandID, Origin: domain.TaskSubmissionOriginDevice,
			DeviceID: &in.DeviceID, ProjectID: p.ProjectID,
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

// SubmitImageRepairTask submits the one maintenance task project-image upkeep
// opens for a repair-class refusal (plan §5.7, Project-image upkeep). The task
// has a daemon origin and names no device; its source is the refusal record's
// v1 preimage, so its source digest is the refusal ID, which the store's
// daemon-origin gate re-checks on write. Its command ID and manual-submission
// identity are domain.ImageUpkeepSubmissionIdentity(refusal ID), so submitting
// the same refusal again returns the recorded submission and opens no second
// task. A refusal of another class is refused before any write, and a project
// with no configured submission policy is store.ErrNotFound from the submitter.
func (s *Service) SubmitImageRepairTask(
	ctx context.Context, projectID domain.ProjectID, refusalID domain.Digest,
) (domain.TaskSubmission, error) {
	if s.blobs == nil || s.taskSubmitter == nil {
		return domain.TaskSubmission{}, fmt.Errorf("submit image repair task: %w", ErrTaskSubmissionUnavailable)
	}
	var refusal domain.ImageUpkeepRefusal
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		refusal, err = tx.GetImageUpkeepRefusal(ctx, refusalID)
		return err
	}); err != nil {
		return domain.TaskSubmission{}, fmt.Errorf("submit image repair task: %w", err)
	}
	if refusal.Class != domain.ImageUpkeepRefusalRepair {
		return domain.TaskSubmission{}, fmt.Errorf("submit image repair task for refusal %s of class %q: %w",
			refusal.ID, refusal.Class, store.ErrDaemonSubmissionUnsourced)
	}
	source, err := refusal.Preimage()
	if err != nil {
		return domain.TaskSubmission{}, err
	}
	commandID := domain.ImageUpkeepSubmissionIdentity(refusal.ID)
	requestBytes, err := json.Marshal(struct {
		Origin    domain.TaskSubmissionOrigin
		ProjectID domain.ProjectID
		RefusalID domain.Digest
	}{domain.TaskSubmissionOriginDaemon, projectID, refusal.ID})
	if err != nil {
		return domain.TaskSubmission{}, err
	}
	requestDigest := domain.Digest(contentaddr.Sum(requestBytes))
	// As in submitTask, the bytes land first so no artifact row names a
	// digest the blob store cannot serve. The store refuses bytes that do not
	// hash to the refusal ID.
	if _, err := s.blobs.Put(refusal.ID, bytes.NewReader(source)); err != nil {
		return domain.TaskSubmission{}, fmt.Errorf("submit image repair task store source: %w", err)
	}
	var submission domain.TaskSubmission
	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		if recorded, getErr := tx.GetTaskSubmission(ctx, commandID); getErr == nil {
			original, err := tx.TaskSubmissionRequestDigest(ctx, commandID)
			if err != nil {
				return err
			}
			if recorded.Origin != domain.TaskSubmissionOriginDaemon || recorded.ProjectID != projectID ||
				recorded.SourceDigest != refusal.ID || original != requestDigest {
				return fmt.Errorf("submit image repair task %q: %w", commandID, store.ErrImmutableConflict)
			}
			submission = recorded
			return errReplay
		} else if !errors.Is(getErr, store.ErrNotFound) {
			return getErr
		}
		outcome, err := s.taskSubmitter.SubmitTask(ctx, tx, TaskSubmissionInput{
			CommandID: commandID, Origin: domain.TaskSubmissionOriginDaemon, RequestDigest: requestDigest,
			ProjectID: projectID, Source: source, SourceDigest: refusal.ID,
			OperatorName: "Repair the project image at " + refusal.TipSHA[:12],
		})
		if err != nil {
			return err
		}
		submission = domain.TaskSubmission{
			CommandID: commandID, Origin: domain.TaskSubmissionOriginDaemon, ProjectID: projectID,
			SourceDigest: refusal.ID, TaskID: outcome.TaskID, SpecificationRunID: outcome.SpecificationRunID,
			Name: outcome.Name,
		}
		return tx.PutTaskSubmissionRequest(ctx, submission, requestDigest)
	})
	if err != nil && !errors.Is(err, errReplay) {
		return domain.TaskSubmission{}, err
	}
	return submission, nil
}

// errEmptyTaskLines rejects a task_lines list that is present but names no
// role: the contract's minItems, which the decoder does not enforce.
var errEmptyTaskLines = errors.New("task_lines must name at least one role when present")

// canonicalTaskLines validates a command's task lines and returns them in
// canonical role order; nil in, nil out. The error is safe to return to the
// client: domain.ValidateTaskLineChoices never puts refused request text in
// one.
func canonicalTaskLines(lines []domain.TaskLineChoice) ([]domain.TaskLineChoice, error) {
	if lines != nil && len(lines) == 0 {
		return nil, errEmptyTaskLines
	}
	return domain.CanonicalTaskLineChoices(lines)
}
