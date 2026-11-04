-- The instance's recorded memory budget (#1595; plan §5.4).
--
-- freesided set-memory-budget writes the one row, and every launch reads it in
-- the transaction that gates it, so a running daemon applies a change at its
-- next admission pass. A recorded budget wins over the startup declaration
-- (-memory-budget-mib, or prod's half of physical memory) on every later start.
-- No row means the startup declaration stands; nothing clears the row.
CREATE TABLE memory_budget (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    budget_mib  INTEGER NOT NULL CHECK (budget_mib > 0),
    recorded_at TEXT    NOT NULL CHECK (recorded_at <> '')
) STRICT;
