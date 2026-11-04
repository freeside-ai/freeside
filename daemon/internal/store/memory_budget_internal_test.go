package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindMemoryBudget returns a store at head to the shape a daemon at 0121
// holds.
func rewindMemoryBudget(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindTaskTriages(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE memory_budget`,
		`DELETE FROM schema_migrations WHERE name = '0122_memory_budget.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func readMemoryBudget(t *testing.T, st *Store) (int, bool) {
	t.Helper()
	var (
		budgetMiB int
		recorded  bool
	)
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		budgetMiB, recorded, err = tx.MemoryBudget(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return budgetMiB, recorded
}

func recordMemoryBudget(st *Store, ctx context.Context, budgetMiB int) error {
	return st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordMemoryBudget(ctx, budgetMiB, slotAt)
	})
}

// TestMemoryBudgetMigration: a daemon at 0121 reads no recorded budget after
// 0122, and the table refuses a second row or a budget below 1 MiB.
func TestMemoryBudgetMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/memory-budget.db", Options{})
	rewindMemoryBudget(t, ctx, st.db)
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)
	if budgetMiB, recorded := readMemoryBudget(t, st); recorded {
		t.Fatalf("budget after the migration = %d MiB, want none recorded", budgetMiB)
	}
	for _, statement := range []string{
		`INSERT INTO memory_budget (id, budget_mib, recorded_at) VALUES (2, 4096, '2026-01-02T03:04:05Z')`,
		`INSERT INTO memory_budget (id, budget_mib, recorded_at) VALUES (1, 0, '2026-01-02T03:04:05Z')`,
		`INSERT INTO memory_budget (id, budget_mib, recorded_at) VALUES (1, 4096, '')`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("%s: accepted", statement)
		}
	}
}

// TestMemoryBudgetRecordsAndReplaces: no budget reads as absent, a recorded
// one replaces the last, and a budget below 1 MiB is refused and leaves the
// recorded one.
func TestMemoryBudgetRecordsAndReplaces(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := openSlotStore(t)
	if _, recorded := readMemoryBudget(t, st); recorded {
		t.Fatal("a fresh store has a recorded budget")
	}
	for _, budgetMiB := range []int{8192, 4096} {
		if err := recordMemoryBudget(st, ctx, budgetMiB); err != nil {
			t.Fatal(err)
		}
		if got, recorded := readMemoryBudget(t, st); !recorded || got != budgetMiB {
			t.Fatalf("budget = %d, %t; want %d", got, recorded, budgetMiB)
		}
	}
	for _, budgetMiB := range []int{0, -1} {
		if err := recordMemoryBudget(st, ctx, budgetMiB); !errors.Is(err, domain.ErrNonPositive) {
			t.Fatalf("record %d MiB = %v, want %v", budgetMiB, err, domain.ErrNonPositive)
		}
	}
	if got, _ := readMemoryBudget(t, st); got != 4096 {
		t.Fatalf("budget after refusals = %d, want 4096", got)
	}
}
