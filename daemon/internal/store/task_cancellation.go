package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrCancellationBinding means a prepared Stop or runtime acknowledgement no
// longer names the current task target. It never clears an existing fence.
var ErrCancellationBinding = errors.New("task cancellation binding changed")

func (tx *ReadTx) cancellationTarget(ctx context.Context, task domain.Task) (domain.TaskCancellationTarget, error) {
	target := domain.TaskCancellationTarget{TaskID: task.ID, ProjectID: task.ProjectID, Runs: []domain.TaskCancellationRun{}}
	if start := task.CurrentStart(); start != nil {
		target.EpisodeOrdinal = start.Ordinal
	}
	ids, err := tx.TaskRunIDs(ctx, task.ID)
	if err != nil {
		return target, err
	}
	for _, id := range ids {
		run, err := tx.GetRun(ctx, id)
		if err != nil {
			return target, err
		}
		if run.TaskID != task.ID || run.ProjectID != task.ProjectID {
			return target, errRowInconsistent
		}
		target.Runs = append(target.Runs, domain.TaskCancellationRun{RunID: id, CampaignID: cancellationCampaign(run.CampaignID)})
	}
	return target, target.Validate()
}

// StopTask persists a new receipt and converges on the current target's fence.
// Call only through Store.Write. The caller must authenticate and recheck the
// active device before replay too. No engine or provider effect occurs here.
func (tx *WriteTx) StopTask(ctx context.Context, request domain.StopTaskRequest, now time.Time) (domain.StopTaskReceipt, Snapshot, error) {
	var empty domain.StopTaskReceipt
	if err := request.Validate(); err != nil {
		return empty, Snapshot{}, err
	}
	if old, snap, err := tx.GetStopTaskReceipt(ctx, request.CommandID); err == nil {
		if old.StopTaskRequest != request {
			return empty, Snapshot{}, ErrImmutableConflict
		}
		return old, snap, nil
	} else if !errors.Is(err, ErrNotFound) {
		return empty, Snapshot{}, err
	}
	for _, query := range []string{`SELECT body FROM commands WHERE command_id = ?`, `SELECT body FROM task_submission_commands WHERE command_id = ?`} {
		body, err := tx.existingBody(ctx, query, request.CommandID)
		if err != nil {
			return empty, Snapshot{}, err
		}
		if body != nil {
			return empty, Snapshot{}, ErrImmutableConflict
		}
	}
	task, err := tx.GetTask(ctx, request.TaskID)
	if err != nil {
		return empty, Snapshot{}, err
	}
	if task.ProjectID != request.ProjectID {
		return empty, Snapshot{}, domain.ErrParentKeyMismatch
	}
	state, err := tx.ServerState(ctx)
	if err != nil {
		return empty, Snapshot{}, err
	}
	// A new Stop is accepted whenever the client saw a revision that exists:
	// unrelated revision movement (constant while an agent runs) no longer
	// rejects it. StopTaskRequest.Validate already requires ExpectedEntityVersion
	// >= 1; a value above the current revision can't have been observed, so it
	// still fails closed. See devlog 2026-09-21 task-stop-live-revision.
	if state.SyncEpoch != request.ExpectedSyncEpoch || request.ExpectedEntityVersion > state.Revision {
		return empty, Snapshot{}, ErrCancellationBinding
	}
	target, err := tx.cancellationTarget(ctx, task)
	if err != nil {
		return empty, Snapshot{}, err
	}
	var requestID string
	err = tx.tx.QueryRowContext(ctx, `SELECT request_id FROM task_cancellations WHERE task_id = ? AND target_digest = ?`, task.ID, target.Digest(state.SyncEpoch)).Scan(&requestID)
	var cancellation domain.TaskCancellation
	if errors.Is(notFoundOr(err), ErrNotFound) {
		cancellation = domain.TaskCancellation{RequestID: "cancel-" + rand.Text(), Target: target, TargetDigest: target.Digest(state.SyncEpoch), SyncEpoch: state.SyncEpoch, FenceRevision: tx.asOfRevision, RequestedAt: now, State: domain.TaskCancellationRequested}
		if err := cancellation.Validate(); err != nil {
			return empty, Snapshot{}, err
		}
		body, err := encode(cancellation)
		if err != nil {
			return empty, Snapshot{}, err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_cancellations VALUES (?, ?, ?, ?, ?)`, cancellation.RequestID, task.ID, cancellation.TargetDigest, tx.asOfRevision, body); err != nil {
			return empty, Snapshot{}, err
		}
	} else if err != nil {
		return empty, Snapshot{}, err
	} else {
		cancellation, err = tx.GetTaskCancellation(ctx, requestID)
		if err != nil {
			return empty, Snapshot{}, err
		}
	}
	receipt := domain.StopTaskReceipt{StopTaskRequest: request, Cancellation: cancellation}
	if err := receipt.Validate(); err != nil {
		return empty, Snapshot{}, err
	}
	body, err := encode(receipt)
	if err != nil {
		return empty, Snapshot{}, err
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO task_stop_commands VALUES (?, ?, ?, ?)`, request.CommandID, cancellation.RequestID, tx.asOfRevision, body)
	return receipt, Snapshot{EntityVersion: 1, AsOfRevision: tx.asOfRevision}, err
}

func (tx *ReadTx) GetStopTaskReceipt(ctx context.Context, commandID string) (domain.StopTaskReceipt, Snapshot, error) {
	var body []byte
	var requestID string
	snap := Snapshot{EntityVersion: 1}
	if err := tx.tx.QueryRowContext(ctx, `SELECT request_id, as_of_revision, body FROM task_stop_commands WHERE command_id = ?`, commandID).Scan(&requestID, &snap.AsOfRevision, &body); err != nil {
		return domain.StopTaskReceipt{}, Snapshot{}, notFoundOr(err)
	}
	r, err := decode[domain.StopTaskReceipt](body)
	if err != nil {
		return r, snap, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if err := r.Validate(); err != nil {
		return r, snap, errRowInconsistent
	}
	c, err := tx.GetTaskCancellation(ctx, requestID)
	if err != nil {
		return r, snap, err
	}
	// Reconstruct the exact acknowledgement snapshot as of receipt acceptance;
	// a changed current state cannot alter the replayed record.
	atAcceptance, err := tx.cancellationAtRevision(ctx, c.RequestID, snap.AsOfRevision)
	if err != nil {
		return r, snap, err
	}
	want, _ := encode(atAcceptance)
	got, _ := encode(r.Cancellation)
	// Re-gate the persisted revision against current server state. AsOfRevision is
	// the accepting write's revision; the global revision is monotonic and is not
	// reset by an epoch change (see NewEpoch), so a coherent receipt's revision is
	// at or below the current one. A future value is single-column corruption:
	// fail closed here rather than replay a fabricated future revision that no
	// revision progress can reach, stranding the pending Stop.
	state, err := tx.ServerState(ctx)
	if err != nil {
		return r, snap, err
	}
	// The receipt records the client's ExpectedEntityVersion unchanged, and the
	// accepting write's revision is strictly greater than any revision the client
	// could have seen, so the recorded version is always below AsOfRevision.
	// Receipts written under the old "exactly one behind" rule still satisfy this.
	if r.CommandID != commandID || r.Cancellation.RequestID != requestID || snap.AsOfRevision < c.FenceRevision || snap.AsOfRevision > state.Revision || r.ExpectedEntityVersion >= snap.AsOfRevision || got != want {
		return r, snap, errRowInconsistent
	}
	return r, snap, nil
}

// GetTaskCancellation re-gates a durable request and its acknowledgement log.
// Historical targets stay readable when later work changes membership.
func (tx *ReadTx) GetTaskCancellation(ctx context.Context, requestID string) (domain.TaskCancellation, error) {
	return tx.cancellationAtRevision(ctx, requestID, 1<<63-1)
}

func (tx *ReadTx) cancellationAtRevision(ctx context.Context, requestID string, revision int64) (domain.TaskCancellation, error) {
	var body []byte
	var taskID domain.TaskID
	var digest domain.Digest
	var fenceRevision int64
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id, target_digest, as_of_revision, body FROM task_cancellations WHERE request_id = ?`, requestID).Scan(&taskID, &digest, &fenceRevision, &body); err != nil {
		return domain.TaskCancellation{}, notFoundOr(err)
	}
	c, err := decode[domain.TaskCancellation](body)
	if err != nil {
		return c, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if c.Validate() != nil || c.RequestID != requestID || c.Target.TaskID != taskID || c.TargetDigest != digest || c.FenceRevision != fenceRevision || c.State != domain.TaskCancellationRequested || fenceRevision > revision {
		return c, errRowInconsistent
	}
	var projectID domain.ProjectID
	if err := tx.tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, taskID).Scan(&projectID); err != nil {
		return c, err
	}
	if projectID != c.Target.ProjectID {
		return c, errRowInconsistent
	}
	ids, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return c, err
	}
	if len(ids) < len(c.Target.Runs) {
		return c, errRowInconsistent
	}
	for i, member := range c.Target.Runs {
		run, err := tx.cancellationRun(ctx, member.RunID)
		if err != nil {
			return c, err
		}
		if ids[i] != member.RunID || run.TaskID != taskID || run.ProjectID != projectID || !sameOptionalCampaign(cancellationCampaign(run.CampaignID), member.CampaignID) {
			return c, errRowInconsistent
		}
	}
	if c.Target.EpisodeOrdinal > 0 {
		var kind domain.TaskLifecycleFactKind
		if err := tx.tx.QueryRowContext(ctx, `SELECT kind FROM task_lifecycle_facts WHERE task_id = ? AND ordinal = ?`, taskID, c.Target.EpisodeOrdinal).Scan(&kind); err != nil {
			return c, err
		}
		if kind != domain.TaskLifecycleStarted {
			return c, errRowInconsistent
		}
	}
	rows, err := tx.tx.QueryContext(ctx, `SELECT id, body FROM task_cancellation_acknowledgements WHERE request_id = ? AND as_of_revision <= ? ORDER BY as_of_revision, rowid`, requestID, revision)
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return c, err
		}
		a, err := decode[domain.TaskCancellationAcknowledgement](b)
		if err != nil {
			return c, fmt.Errorf("%w: %w", errRowInconsistent, err)
		}
		if a.ID != id || c.State == domain.TaskCancellationConfirmed || (c.Acknowledgement != nil && a.RecordedAt.Before(c.Acknowledgement.RecordedAt)) {
			return c, errRowInconsistent
		}
		c.State, c.Acknowledgement = a.State, &a
		if c.Validate() != nil {
			return c, errRowInconsistent
		}
	}
	return c, rows.Err()
}

func sameOptionalCampaign(a, b *domain.CampaignID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (tx *ReadTx) currentTaskCancellation(ctx context.Context, taskID domain.TaskID) (*domain.TaskCancellation, error) {
	var id string
	err := tx.tx.QueryRowContext(ctx, `SELECT request_id FROM task_cancellations WHERE task_id = ? ORDER BY as_of_revision DESC LIMIT 1`, taskID).Scan(&id)
	if err != nil {
		if errors.Is(notFoundOr(err), ErrNotFound) {
			return nil, nil
		}
		// Task backfills execute before this additive migration.
		if exists, checkErr := tx.tableExists(ctx, "task_cancellations"); checkErr == nil && !exists {
			return nil, nil
		}
		return nil, err
	}
	c, err := tx.GetTaskCancellation(ctx, id)
	return &c, err
}

// AcknowledgeTaskCancellation accepts only a bound daemon result. The bool is
// false for an exact replay, allowing callers to roll back a no-op Write and
// preserve the revision. No production caller exists until runtime enforcement.
func (tx *WriteTx) AcknowledgeTaskCancellation(ctx context.Context, ack domain.TaskCancellationAcknowledgement) (bool, error) {
	if err := ack.Validate(); err != nil {
		return false, err
	}
	body, err := encode(ack)
	if err != nil {
		return false, err
	}
	old, err := tx.existingBody(ctx, `SELECT body FROM task_cancellation_acknowledgements WHERE id = ?`, ack.ID)
	if err != nil {
		return false, err
	}
	if old != nil {
		if string(old) != body {
			return false, ErrImmutableConflict
		}
		_, err := tx.GetTaskCancellation(ctx, ack.RequestID)
		return false, err
	}
	c, err := tx.GetTaskCancellation(ctx, ack.RequestID)
	if err != nil {
		return false, err
	}
	task, err := tx.GetTask(ctx, c.Target.TaskID)
	if err != nil {
		return false, err
	}
	target, err := tx.cancellationTarget(ctx, task)
	if err != nil {
		return false, err
	}
	state, err := tx.ServerState(ctx)
	if err != nil {
		return false, err
	}
	if ack.TargetDigest != c.TargetDigest || target.Digest(state.SyncEpoch) != c.TargetDigest {
		return false, ErrCancellationBinding
	}
	if c.State == domain.TaskCancellationConfirmed || (c.Acknowledgement != nil && ack.RecordedAt.Before(c.Acknowledgement.RecordedAt)) {
		return false, domain.ErrImmutableTransition
	}
	c.State, c.Acknowledgement = ack.State, &ack
	if err := c.Validate(); err != nil {
		return false, err
	}
	// The current target was rechecked above under this same write lock. Release
	// only its still-held episode, before inserting confirmation so TaskWIP still
	// reports the held slot. No start and completed episodes append no fact.
	if ack.State == domain.TaskCancellationConfirmed && domain.TaskWIP(task) {
		if _, err := tx.AbandonTask(ctx, task.ID, ack.RecordedAt); err != nil {
			return false, err
		}
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO task_cancellation_acknowledgements VALUES (?, ?, ?, ?)`, ack.ID, ack.RequestID, tx.asOfRevision, body)
	if err == nil {
		_, err = tx.tx.ExecContext(ctx, `UPDATE tasks SET entity_version = entity_version + 1, as_of_revision = ? WHERE id = ?`, tx.asOfRevision, task.ID)
	}
	return err == nil, err
}

func (tx *WriteTx) rejectStopCommandID(ctx context.Context, id string) error {
	if exists, err := tx.tableExists(ctx, "task_stop_commands"); err != nil {
		return err
	} else if !exists {
		return nil
	}
	if body, err := tx.existingBody(ctx, `SELECT body FROM task_stop_commands WHERE command_id = ?`, id); err != nil {
		return err
	} else if body != nil {
		return fmt.Errorf("command %q: %w", id, ErrImmutableConflict)
	}
	return nil
}

func cancellationCampaign(id domain.CampaignID) *domain.CampaignID {
	if id == "" {
		return nil
	}
	return &id
}

// Read only the membership authority here: GetRun re-gates its task and would
// recursively reconstruct this cancellation. The full run gate still applies
// when capturing a new target and whenever consumers load executions.
func (tx *ReadTx) cancellationRun(ctx context.Context, id domain.RunID) (domain.Run, error) {
	var body []byte
	var taskID domain.TaskID
	var projectID domain.ProjectID
	var campaign sql.NullString
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id, project_id, campaign_id, body FROM runs WHERE id = ?`, id).Scan(&taskID, &projectID, &campaign, &body); err != nil {
		return domain.Run{}, notFoundOr(err)
	}
	run, err := decode[domain.Run](body)
	if err != nil {
		return run, err
	}
	if run.Validate() != nil || run.ID != id || run.TaskID != taskID || run.ProjectID != projectID || string(run.CampaignID) != campaign.String {
		return run, errRowInconsistent
	}
	return run, nil
}
