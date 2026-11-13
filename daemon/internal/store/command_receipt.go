package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Migration 0118 adds client_command_receipts: one row per accepted operator
// command, keyed by command_id, holding the receipt arm's JSON under its kind
// and the revision of the transaction that accepted it.

// RecordCommandReceipt records an accepted operator command's receipt in the
// transaction that applies its effect, and returns the receipt's sync
// metadata. It is client-visible, so it runs only inside Write. A command_id
// is unique across kinds: one already recorded as a decision, a submission, a
// Stop, or another receipt is ErrImmutableConflict. The caller replays a
// recorded receipt through GetCommandReceipt before applying anything.
func (tx *WriteTx) RecordCommandReceipt(ctx context.Context, receipt domain.CommandReceipt) (Snapshot, error) {
	if err := receipt.Validate(); err != nil {
		return Snapshot{}, fmt.Errorf("record command receipt: %w", err)
	}
	commandID := receipt.CommandID()
	if err := tx.RequireCommandIDFree(ctx, commandID); err != nil {
		return Snapshot{}, fmt.Errorf("record command receipt: %w", err)
	}
	body, err := encodeCommandReceipt(receipt)
	if err != nil {
		return Snapshot{}, fmt.Errorf("record command receipt %q: %w", commandID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO client_command_receipts VALUES (?, ?, ?, ?)`,
		commandID, receipt.Kind(), tx.asOfRevision, body); err != nil {
		return Snapshot{}, fmt.Errorf("record command receipt %q: %w", commandID, err)
	}
	return Snapshot{EntityVersion: 1, AsOfRevision: tx.asOfRevision}, nil
}

// RequireCommandIDFree refuses a command_id any command kind already recorded
// (a decision, a submission, a Stop, or a receipt) with ErrImmutableConflict.
// An operator command runs it before applying its effect, so a reused id is
// refused before anything else can answer for it.
func (tx *WriteTx) RequireCommandIDFree(ctx context.Context, commandID string) error {
	if err := tx.rejectRecordedCommandID(ctx, commandID); err != nil {
		return err
	}
	for _, query := range []string{
		`SELECT body FROM commands WHERE command_id = ?`,
		`SELECT body FROM task_submission_commands WHERE command_id = ?`,
	} {
		if body, err := tx.existingBody(ctx, query, commandID); err != nil {
			return fmt.Errorf("command %q: %w", commandID, err)
		} else if body != nil {
			return fmt.Errorf("command %q: %w", commandID, ErrImmutableConflict)
		}
	}
	return tx.rejectStopCommandID(ctx, commandID)
}

// GetCommandReceipt reads a recorded receipt, re-validating it and holding its
// key, kind, and revision to the row: ErrNotFound when none is recorded.
func (tx *ReadTx) GetCommandReceipt(ctx context.Context, commandID string) (domain.CommandReceipt, Snapshot, error) {
	var (
		kind domain.CommandKind
		body []byte
	)
	snap := Snapshot{EntityVersion: 1}
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT kind, as_of_revision, body FROM client_command_receipts WHERE command_id = ?`, commandID).
		Scan(&kind, &snap.AsOfRevision, &body); err != nil {
		return domain.CommandReceipt{}, Snapshot{}, notFoundOr(err)
	}
	receipt, err := decodeCommandReceipt(kind, body)
	if err != nil {
		return domain.CommandReceipt{}, Snapshot{}, fmt.Errorf("command receipt %q: %w: %w", commandID, errRowInconsistent, err)
	}
	if receipt.CommandID() != commandID {
		return domain.CommandReceipt{}, Snapshot{}, fmt.Errorf("command receipt %q: %w", commandID, errRowInconsistent)
	}
	state, err := tx.ServerState(ctx)
	if err != nil {
		return domain.CommandReceipt{}, Snapshot{}, err
	}
	// The accepting revision is one the store has reached; a row ahead of the
	// server revision did not come from this store's history.
	if snap.AsOfRevision > state.Revision {
		return domain.CommandReceipt{}, Snapshot{}, fmt.Errorf("command receipt %q revision %d ahead of %d: %w",
			commandID, snap.AsOfRevision, state.Revision, errRowInconsistent)
	}
	return receipt, snap, nil
}

// rejectRecordedCommandID refuses a command_id that already holds a receipt,
// so the decision, submission, and Stop writers keep command_id unique across
// every kind. Before migration 0118 the table does not exist and nothing can
// collide.
func (tx *WriteTx) rejectRecordedCommandID(ctx context.Context, commandID string) error {
	if exists, err := tx.tableExists(ctx, "client_command_receipts"); err != nil {
		return err
	} else if !exists {
		return nil
	}
	if body, err := tx.existingBody(ctx,
		`SELECT body FROM client_command_receipts WHERE command_id = ?`, commandID); err != nil {
		return err
	} else if body != nil {
		return fmt.Errorf("command %q: %w", commandID, ErrImmutableConflict)
	}
	return nil
}

func encodeCommandReceipt(receipt domain.CommandReceipt) (string, error) {
	switch receipt.Kind() {
	case domain.CommandKindRefreshUsage:
		return encode(receipt.UsageRefresh)
	case domain.CommandKindReassignCollector:
		return encode(receipt.CollectorReassignment)
	case domain.CommandKindChangeTaskLine:
		return encode(receipt.TaskLineChange)
	case domain.CommandKindSetRoutingPolicy:
		return encode(receipt.RoutingPolicyChange)
	case domain.CommandKindMoveParkedTask:
		return encode(receipt.ParkedTaskMove)
	case domain.CommandKindSetWorkflowPolicy:
		return encode(receipt.WorkflowPolicyChange)
	case domain.CommandKindDecision, domain.CommandKindSubmitTask, domain.CommandKindStopTask:
	}
	return "", domain.ErrCommandReceiptArms
}

func decodeCommandReceipt(kind domain.CommandKind, body []byte) (domain.CommandReceipt, error) {
	switch kind {
	case domain.CommandKindRefreshUsage:
		arm, err := decode[domain.UsageRefreshReceipt](body)
		return domain.CommandReceipt{UsageRefresh: &arm}, err
	case domain.CommandKindReassignCollector:
		arm, err := decode[domain.CollectorReassignmentReceipt](body)
		return domain.CommandReceipt{CollectorReassignment: &arm}, err
	case domain.CommandKindChangeTaskLine:
		arm, err := decode[domain.TaskLineChangeReceipt](body)
		return domain.CommandReceipt{TaskLineChange: &arm}, err
	case domain.CommandKindSetRoutingPolicy:
		arm, err := decode[domain.RoutingPolicyChangeReceipt](body)
		return domain.CommandReceipt{RoutingPolicyChange: &arm}, err
	case domain.CommandKindMoveParkedTask:
		arm, err := decode[domain.ParkedTaskMoveReceipt](body)
		return domain.CommandReceipt{ParkedTaskMove: &arm}, err
	case domain.CommandKindSetWorkflowPolicy:
		arm, err := decode[domain.WorkflowPolicyChangeReceipt](body)
		return domain.CommandReceipt{WorkflowPolicyChange: &arm}, err
	case domain.CommandKindDecision, domain.CommandKindSubmitTask, domain.CommandKindStopTask:
	}
	return domain.CommandReceipt{}, fmt.Errorf("kind %q: %w", kind, domain.ErrInvalidCommandKind)
}
