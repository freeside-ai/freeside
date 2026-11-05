package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Task attachments (migration 0124; plan §5.15) are the files a submission
// carried, in submitted order. They are written once, in the transaction that
// creates the task, and never change. They are operator input, not evidence,
// so they live beside the task rather than among the artifacts.

const taskAttachmentColumns = `task_id, position, digest, name, media_type, claimed_media_type, size_bytes`

// PutTaskAttachments records a task's attachments in the given order. The
// whole list is validated before any row is written, and a task that already
// has attachments is refused with ErrImmutableConflict: the list is part of
// the submission that created the task, and a replayed submission returns its
// recorded result before reaching here. An empty list writes nothing.
func (tx *WriteTx) PutTaskAttachments(ctx context.Context, taskID domain.TaskID, list []domain.TaskAttachment) error {
	if len(list) == 0 {
		return nil
	}
	if err := domain.ValidateTaskAttachments(list); err != nil {
		return fmt.Errorf("task attachments %q: %w", taskID, err)
	}
	var existing int
	if err := tx.tx.QueryRowContext(ctx, `SELECT count(*) FROM task_attachments WHERE task_id = ?`, taskID).
		Scan(&existing); err != nil {
		return fmt.Errorf("task attachments %q: %w", taskID, err)
	}
	if existing != 0 {
		return fmt.Errorf("task attachments %q: %w", taskID, ErrImmutableConflict)
	}
	for position, a := range list {
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_attachments (`+taskAttachmentColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			taskID, position, a.Digest, a.Name, a.MediaType, a.ClaimedMediaType, a.SizeBytes); err != nil {
			return fmt.Errorf("task attachment %q %d: %w", taskID, position, err)
		}
	}
	return nil
}

// TaskAttachments returns the task's attachments in submitted order, none for
// a task without any. It re-validates the whole list, and the positions must
// run from 0 without a gap, so an edited or missing row fails the read loudly
// rather than serving a list the submission never carried.
func (tx *ReadTx) TaskAttachments(ctx context.Context, taskID domain.TaskID) ([]domain.TaskAttachment, error) {
	type row struct {
		position   int
		attachment domain.TaskAttachment
	}
	rows, err := queryAll(ctx, tx.tx, func(s scanner) (row, error) {
		var (
			r      row
			ignore domain.TaskID
		)
		err := s.Scan(&ignore, &r.position, &r.attachment.Digest, &r.attachment.Name,
			&r.attachment.MediaType, &r.attachment.ClaimedMediaType, &r.attachment.SizeBytes)
		return r, err
	}, `SELECT `+taskAttachmentColumns+` FROM task_attachments WHERE task_id = ? ORDER BY position`, taskID)
	if err != nil {
		return nil, fmt.Errorf("task attachments %q: %w", taskID, err)
	}
	list := make([]domain.TaskAttachment, len(rows))
	for i, r := range rows {
		if r.position != i {
			return nil, fmt.Errorf("task attachments %q position %d: %w", taskID, r.position, errRowInconsistent)
		}
		list[i] = r.attachment
	}
	if err := domain.ValidateTaskAttachments(list); err != nil {
		return nil, fmt.Errorf("task attachments %q: %w: %w", taskID, errRowInconsistent, err)
	}
	return list, nil
}
