package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Task triages (migration 0122; plan §5.13, Specifier Triage) are one row per
// task, written once and never updated. The record is Tier B state: it
// decodes with plain validation and a bad row fails loudly. Admission's
// anchor of a triage-sourced task line is the Tier A gate that reads it.

// RecordTaskTriage stores the task's triage record. A task holds one: a
// second record for the task is refused with domain.ErrDuplicate, because a
// later attempt reads the recorded outcome and never triages again.
func (tx *WriteTx) RecordTaskTriage(ctx context.Context, record domain.TaskTriage) error {
	if err := record.Validate(); err != nil {
		return fmt.Errorf("record task triage %q: %w", record.TaskID, err)
	}
	if _, found, err := tx.GetTaskTriage(ctx, record.TaskID); err != nil {
		return fmt.Errorf("record task triage %q: %w", record.TaskID, err)
	} else if found {
		return fmt.Errorf("record task triage %q: %w", record.TaskID, domain.ErrDuplicate)
	}
	var tier any
	if record.Tier != nil {
		tier = string(*record.Tier)
	}
	var reason any
	if record.Reason != nil {
		reason = *record.Reason
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_triages
		(task_id, outcome, tier, reason, escalated, recorded_at) VALUES (?, ?, ?, ?, ?, ?)`,
		record.TaskID, record.Outcome, tier, reason, record.Escalated, formatTime(record.RecordedAt)); err != nil {
		return fmt.Errorf("record task triage %q: %w", record.TaskID, err)
	}
	return nil
}

// GetTaskTriage returns the task's triage record and whether it has one.
func (tx *ReadTx) GetTaskTriage(ctx context.Context, taskID domain.TaskID) (domain.TaskTriage, bool, error) {
	var (
		outcome, recordedAt string
		tier, reason        sql.NullString
		escalated           bool
	)
	err := tx.tx.QueryRowContext(ctx, `SELECT outcome, tier, reason, escalated, recorded_at
		FROM task_triages WHERE task_id = ?`, taskID).Scan(&outcome, &tier, &reason, &escalated, &recordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskTriage{}, false, nil
	}
	if err != nil {
		return domain.TaskTriage{}, false, fmt.Errorf("task triage %q: %w", taskID, err)
	}
	at, err := parseTime(recordedAt)
	if err != nil {
		return domain.TaskTriage{}, false, fmt.Errorf("task triage %q recorded_at: %w", taskID, err)
	}
	record := domain.TaskTriage{
		TaskID: taskID, Outcome: domain.TriageOutcome(outcome), Escalated: escalated, RecordedAt: at,
	}
	if tier.Valid {
		record.Tier = new(domain.TriageTier(tier.String))
	}
	if reason.Valid {
		record.Reason = new(reason.String)
	}
	if err := record.Validate(); err != nil {
		return domain.TaskTriage{}, false, fmt.Errorf("task triage %q: %w", taskID, err)
	}
	return record, true, nil
}

// RecordSpecifierTriage stores the task's triage record and, when the record
// escalates, the specifier's triage line naming counterpart, in the caller's
// transaction (plan §5.13, Specifier Triage). The line is set at the record's
// instant and must be the specifier's first version: triage runs only for a
// specifier with no line, so a role that gained one since the engine read
// fails the whole write rather than stacking a triage version on an
// operator's choice. A record that does not escalate takes no counterpart
// and writes no line.
//
// Every outcome advances the task's own version, so sync serves the task and
// its timeline afresh. It returns the line, or nil when none was written.
func (tx *WriteTx) RecordSpecifierTriage(
	ctx context.Context, record domain.TaskTriage, counterpart string,
) (*domain.TaskLine, error) {
	if record.Escalated != (counterpart != "") {
		return nil, fmt.Errorf("record specifier triage %q: escalated %t with counterpart %q: %w",
			record.TaskID, record.Escalated, counterpart, domain.ErrTaskTriageInconsistent)
	}
	if err := tx.RecordTaskTriage(ctx, record); err != nil {
		return nil, err
	}
	var line *domain.TaskLine
	if record.Escalated {
		appended, err := tx.AppendTaskLine(ctx, domain.TaskLineInput{
			TaskID: record.TaskID, Role: domain.RoleSpecifier, Agent: counterpart,
			Source: domain.TaskLineSourceTriage, SetBy: string(record.TaskID),
		}, record.RecordedAt)
		if err != nil {
			return nil, err
		}
		if appended.Version != 1 {
			return nil, fmt.Errorf("record specifier triage %q: the specifier already has a line: %w",
				record.TaskID, domain.ErrTaskLineInconsistent)
		}
		line = &appended
	}
	task, err := tx.GetTask(ctx, record.TaskID)
	if err != nil {
		return nil, fmt.Errorf("record specifier triage %q: %w", record.TaskID, err)
	}
	if err := tx.updateTask(ctx, task); err != nil {
		return nil, fmt.Errorf("record specifier triage %q: %w", record.TaskID, err)
	}
	return line, nil
}
