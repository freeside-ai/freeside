package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	memoryBudgetSQL       = `SELECT budget_mib FROM memory_budget WHERE id = 1`
	recordMemoryBudgetSQL = `
INSERT INTO memory_budget (id, budget_mib, recorded_at) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET budget_mib = excluded.budget_mib, recorded_at = excluded.recorded_at`
)

// MemoryBudget returns the instance's recorded memory budget in MiB, and
// false when none is recorded, in which case the startup declaration stands
// (plan §5.4). A launch reads it in the transaction that gates it, so a
// recorded change applies from the next admission pass.
func (tx *ReadTx) MemoryBudget(ctx context.Context) (int, bool, error) {
	var budgetMiB int
	err := tx.tx.QueryRowContext(ctx, memoryBudgetSQL).Scan(&budgetMiB)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read memory budget: %w", err)
	}
	// The column's CHECK already refuses this; a row that slipped past it
	// fails the gate closed rather than reserving against no budget.
	if budgetMiB < 1 {
		return 0, false, fmt.Errorf("read memory budget: %d MiB: %w", budgetMiB, domain.ErrNonPositive)
	}
	return budgetMiB, true, nil
}

// RecordMemoryBudget records the instance's memory budget, replacing any
// recorded one. It limits only reservations made after it; memory already
// reserved stays reserved. The budget is daemon configuration no client
// syncs, so it takes no revision; a launch it refuses or holds records that
// on the run, which does.
func (tx *InternalTx) RecordMemoryBudget(ctx context.Context, budgetMiB int, at time.Time) error {
	if budgetMiB < 1 {
		return fmt.Errorf("record memory budget %d MiB: %w", budgetMiB, domain.ErrNonPositive)
	}
	if _, err := tx.tx.ExecContext(ctx, recordMemoryBudgetSQL, budgetMiB, formatTime(at)); err != nil {
		return fmt.Errorf("record memory budget: %w", err)
	}
	return nil
}
