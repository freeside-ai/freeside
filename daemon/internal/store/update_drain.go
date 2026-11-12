package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordUpdateDrainTransitionSQL = `
INSERT INTO update_drain_transitions (state, build, forced, occurred_at)
VALUES (?, ?, ?, ?)`
	latestUpdateDrainTransitionSQL = `
SELECT state, build, forced, occurred_at
FROM update_drain_transitions ORDER BY id DESC LIMIT 1`
)

// RecordUpdateDrainTransition appends one update drain row (issue #3103).
// Append-only: the latest row is the drain state, and a repeated state is a
// real recorded request, not a conflict. The row holds work and grants
// nothing, so it is validated and written with no command binding.
func (tx *InternalTx) RecordUpdateDrainTransition(
	ctx context.Context, transition domain.UpdateDrainTransition,
) error {
	if err := transition.Validate(); err != nil {
		return fmt.Errorf("record update drain transition: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordUpdateDrainTransitionSQL,
		transition.State, transition.Build, transition.Forced,
		formatTime(transition.OccurredAt)); err != nil {
		return fmt.Errorf("record update drain transition: %w", err)
	}
	return nil
}

// ErrUpdateDrainInForce refuses a new launch while an update drain is in
// force (issue #3104). It is a wait, not a failure: the launch stays pending
// and starts once the drain lifts.
var ErrUpdateDrainInForce = errors.New("an update drain is in force")

// RequireNoUpdateDrain refuses with ErrUpdateDrainInForce while a drain is in
// force. Every launch site runs it, in every operating mode: unlike the
// operator stop, a drain holds attended work too, because a restart would cut
// it short all the same.
func (tx *ReadTx) RequireNoUpdateDrain(ctx context.Context) error {
	drain, err := tx.UpdateDrainInForce(ctx)
	if err != nil {
		return err
	}
	if drain != nil {
		return ErrUpdateDrainInForce
	}
	return nil
}

// ClearUpdateDrainHolds removes every run's hold observation that names
// update_drain. Lifting a drain ends every such wait at once; a hold that
// names another cause stays as it is.
func (tx *WriteTx) ClearUpdateDrainHolds(ctx context.Context) error {
	if _, err := tx.tx.ExecContext(ctx,
		`DELETE FROM run_hold_observations WHERE reason = ?`, domain.HoldUpdateDrain); err != nil {
		return fmt.Errorf("clear update drain holds: %w", err)
	}
	return nil
}

// UpdateDrainInForce returns the latest drain row when it is draining, and
// nil when the log is empty or the latest row lifted the drain.
func (tx *ReadTx) UpdateDrainInForce(ctx context.Context) (*domain.UpdateDrainTransition, error) {
	latest, found, err := tx.latestUpdateDrainTransition(ctx)
	if err != nil || !found || latest.State != domain.UpdateDrainDraining {
		return nil, err
	}
	return &latest, nil
}

// latestUpdateDrainTransition reads the newest drain row, with presence
// reported separately because an empty log is the legitimate "no drain"
// state. A row the current vocabulary can't express fails the read loudly
// (Tier B).
func (tx *ReadTx) latestUpdateDrainTransition(
	ctx context.Context,
) (domain.UpdateDrainTransition, bool, error) {
	var (
		state      string
		build      string
		forced     bool
		occurredAt string
	)
	err := tx.tx.QueryRowContext(ctx, latestUpdateDrainTransitionSQL).
		Scan(&state, &build, &forced, &occurredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.UpdateDrainTransition{}, false, nil
	case err != nil:
		return domain.UpdateDrainTransition{}, false, fmt.Errorf("latest update drain transition: %w", err)
	}
	at, err := parseTime(occurredAt)
	if err != nil {
		return domain.UpdateDrainTransition{}, false,
			fmt.Errorf("latest update drain transition occurred_at %q: %w", occurredAt, err)
	}
	transition := domain.UpdateDrainTransition{
		State:      domain.UpdateDrainState(state),
		Build:      domain.Digest(build),
		Forced:     forced,
		OccurredAt: at,
	}
	if err := transition.Validate(); err != nil {
		return domain.UpdateDrainTransition{}, false, fmt.Errorf("latest update drain transition: %w", err)
	}
	return transition, true, nil
}
