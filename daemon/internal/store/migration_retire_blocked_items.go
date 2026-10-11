package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

const retireBlockedItemsMigration = "0096_retire_blocked_items.sql"

// retireOpenBlockedItems closes every open blocked item ahead of a backfill
// that decodes attention items and skips the rows it cannot decode. The
// domain refuses an open blocked item, so a store that reaches such a
// backfill with one would leave the row without the decision surface or task
// binding every later read requires, and one unreadable row fails the whole
// inbox listing. Applying migration 0096's own statements first shows the
// backfill the row as 0096 leaves it. 0096 then finds no open row and changes
// nothing.
func retireOpenBlockedItems(ctx context.Context, tx *sql.Tx) error {
	statements, err := fs.ReadFile(migrations.FS, retireBlockedItemsMigration)
	if err != nil {
		return fmt.Errorf("read %s: %w", retireBlockedItemsMigration, err)
	}
	if _, err := tx.ExecContext(ctx, string(statements)); err != nil {
		return fmt.Errorf("retire open blocked items: %w", err)
	}
	return nil
}
