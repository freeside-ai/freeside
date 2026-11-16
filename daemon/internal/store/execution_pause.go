package store

import (
	"context"
	"errors"
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

// ErrExecutionPauseInForce refuses new work while an execution pause is in
// force, requested or released. Like ErrUpdateDrainInForce it is a wait, not
// a failure: the work stays pending and starts once the pause clears.
var ErrExecutionPauseInForce = errors.New("an execution pause is in force")

// RequireNewWorkOpen refuses new work while an update drain is in force
// (ErrUpdateDrainInForce) or an execution pause is (ErrExecutionPauseInForce).
// Every site that starts new work runs it, in every operating mode, because
// both wait for running work to reach zero and new work would put that off.
func (tx *ReadTx) RequireNewWorkOpen(ctx context.Context) error {
	if err := tx.RequireNoUpdateDrain(ctx); err != nil {
		return err
	}
	pause, err := tx.ExecutionPause(ctx)
	if err != nil {
		return err
	}
	if pause != nil {
		return ErrExecutionPauseInForce
	}
	return nil
}

// NewWorkClosed reports whether err is RequireNewWorkOpen's refusal: a wait
// for a drain or a pause to lift, not a failure.
func NewWorkClosed(err error) bool {
	return errors.Is(err, ErrUpdateDrainInForce) || errors.Is(err, ErrExecutionPauseInForce)
}
