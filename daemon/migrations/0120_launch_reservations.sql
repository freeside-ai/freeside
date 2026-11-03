-- Launch memory reservations, launch waits, and the machine_capacity hold's
-- fact (#1598; plan §5.4, revision 105).
--
-- A reservation is one launch's declared memory plus the runtime's per-VM
-- overhead, written in the transaction that takes the launch's usage-pool slot
-- (a verification job takes none). Whether it still counts is derived: an
-- execution or review reservation holds while its admission holds its slot,
-- and a verification reservation until released_at, written once its
-- containers are proved absent. released_at on an execution or review row is
-- only the gate's note that the slot has ended, so later gates skip the row;
-- re-reserving the same invocation clears it.
CREATE TABLE launch_reservations (
    invocation_id TEXT    NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT    NOT NULL CHECK (run_id <> ''),
    kind          TEXT    NOT NULL CHECK (kind IN ('execution', 'review', 'verification')),
    memory_mib    INTEGER NOT NULL CHECK (memory_mib > 0),
    overhead_mib  INTEGER NOT NULL CHECK (overhead_mib >= 0),
    reserved_at   TEXT    NOT NULL CHECK (reserved_at <> ''),
    released_at   TEXT    CHECK (released_at IS NULL OR released_at <> '')
) STRICT;

CREATE INDEX launch_reservations_open ON launch_reservations (released_at);

-- A launch refused only for memory waits here, in its task's submission
-- order, until a gate grants it. The row is the durable order the gate reads;
-- the run's hold is observability only. A launch refused for anything else
-- leaves the order. refused_at is the latest memory refusal: a launch still
-- pursued is refused again every pass, so a wait whose run can no longer
-- launch, or whose refusal has lapsed because nothing pursues the launch any
-- more, is ignored and deleted by the next gate.
CREATE TABLE launch_waits (
    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id          TEXT NOT NULL CHECK (run_id <> ''),
    task_created_at TEXT NOT NULL CHECK (task_created_at <> ''),
    waiting_since   TEXT NOT NULL CHECK (waiting_since <> ''),
    refused_at      TEXT NOT NULL CHECK (refused_at <> '')
) STRICT;

-- The machine_capacity hold's fact. Validate on the reader ties the columns to
-- the reason and fails closed, as for the 0119 quota columns. Nothing is
-- backfilled: the reason is new with these columns.
ALTER TABLE run_hold_observations ADD COLUMN capacity_reservation_mib INTEGER
    CHECK (capacity_reservation_mib IS NULL OR capacity_reservation_mib > 0);
ALTER TABLE run_hold_observations ADD COLUMN capacity_reserved_mib INTEGER
    CHECK (capacity_reserved_mib IS NULL OR capacity_reserved_mib >= 0);
ALTER TABLE run_hold_observations ADD COLUMN capacity_budget_mib INTEGER
    CHECK (capacity_budget_mib IS NULL OR capacity_budget_mib > 0);
ALTER TABLE run_hold_observations ADD COLUMN capacity_ahead INTEGER
    CHECK (capacity_ahead IS NULL OR capacity_ahead >= 0);
