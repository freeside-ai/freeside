package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func executionOutcomeStatus(status exec.Status) (domain.ExecutionOutcomeStatus, error) {
	switch status {
	case exec.StatusFailed:
		return domain.ExecutionOutcomeFailed, nil
	case exec.StatusCanceled:
		return domain.ExecutionOutcomeCanceled, nil
	case exec.StatusGone:
		return domain.ExecutionOutcomeLost, nil
	case exec.StatusPending, exec.StatusRunning, exec.StatusCompleted, exec.StatusBlocked:
		return "", fmt.Errorf("execution status %q has no failure outcome", status)
	}
	return "", fmt.Errorf("unknown execution status %q", status)
}

func executionFailureFacts(
	ctx context.Context,
	tx *store.WriteTx,
	invocationID domain.InvocationID,
	status exec.Status,
	summary string,
	recordedAt time.Time,
	stage domain.StageName,
) (*domain.ExecutionFailureFacts, error) {
	admission, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	mapped, err := executionOutcomeStatus(status)
	if err != nil {
		return nil, err
	}
	outcome, err := tx.GetExecutionOutcomeRecord(ctx, invocationID)
	if err == nil {
		if outcome.Status != mapped {
			return nil, fmt.Errorf("execution outcome %q disagrees with terminal status: %w",
				invocationID, domain.ErrParentKeyMismatch)
		}
		return &domain.ExecutionFailureFacts{
			Outcome: mapped, Stage: stage, InvocationID: invocationID,
		}, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if _, exportErr := tx.GetExecutionExportRecord(ctx, invocationID); exportErr == nil {
		// A completed execution can fail later while its output is interpreted.
		// The immutable export is its terminal authority, so no non-export
		// failure fact can be minted for that card.
		return nil, nil
	} else if !errors.Is(exportErr, store.ErrNotFound) {
		return nil, exportErr
	}
	if mapped == domain.ExecutionOutcomeLost {
		summary = ""
	}
	if err := tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: invocationID, AdmissionID: admission.ID,
		Status: mapped, Summary: summary, RecordedAt: recordedAt,
	}); err != nil {
		return nil, err
	}
	return &domain.ExecutionFailureFacts{
		Outcome: mapped, Stage: stage, InvocationID: invocationID,
	}, nil
}
