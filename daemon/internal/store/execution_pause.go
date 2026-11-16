package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ExecutionPause returns the operator's pause of prod's execution, or nil
// when none is in force. It reads the instance settings' execution_pause
// section alone, so a store without the sections a run needs still answers.
func (tx *ReadTx) ExecutionPause(ctx context.Context) (*domain.ExecutionPauseSettings, error) {
	record, err := tx.InstanceSettingsRecord(ctx)
	if err != nil {
		return nil, err
	}
	pause, err := record.ExecutionPause()
	if err != nil {
		return nil, fmt.Errorf("execution pause: %w", err)
	}
	return pause, nil
}
