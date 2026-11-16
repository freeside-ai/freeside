-- Reservations and waits for the daemon's own work (issue #3206; plan §5.4,
-- revision 125). Every container the daemon starts on its own, such as an
-- image upkeep build and its probes, a ward proof pass, or a model discovery
-- probe, reserves memory against the budget as a launch does, but no run asks
-- for it.
--
-- launch_reservations is rebuilt so run_id may be NULL and kind carries no
-- CHECK list: LaunchReservation.Validate refuses an unknown kind, a launch
-- kind (execution, review, verification) with no run, and a daemon kind
-- (image_build, image_registry, proof_pass, probe) with one, on write and on
-- decode. A daemon reservation holds until its explicit release, as a
-- verification reservation does.
--
-- launch_waits is rebuilt so a wait may name no run and no task. The memory
-- gate orders such a wait ahead of every task launch, and daemon waits among
-- themselves by waiting_since. A run's wait still names both its run and its
-- task's submission instant, so the two columns are NULL together.
--
-- Both rebuilds copy every row unchanged, open and released reservations and
-- live waits alike, and recreate the open-reservation index. Nothing reads
-- either table under a foreign key.

CREATE TABLE launch_reservations_rebuilt (
    invocation_id TEXT    NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT    CHECK (run_id IS NULL OR run_id <> ''),
    kind          TEXT    NOT NULL CHECK (kind <> ''),
    memory_mib    INTEGER NOT NULL CHECK (memory_mib > 0),
    overhead_mib  INTEGER NOT NULL CHECK (overhead_mib >= 0),
    reserved_at   TEXT    NOT NULL CHECK (reserved_at <> ''),
    released_at   TEXT    CHECK (released_at IS NULL OR released_at <> '')
) STRICT;

INSERT INTO launch_reservations_rebuilt
    (invocation_id, run_id, kind, memory_mib, overhead_mib, reserved_at, released_at)
SELECT invocation_id, run_id, kind, memory_mib, overhead_mib, reserved_at, released_at
FROM launch_reservations;

DROP TABLE launch_reservations;
ALTER TABLE launch_reservations_rebuilt RENAME TO launch_reservations;
CREATE INDEX launch_reservations_open ON launch_reservations (released_at);

CREATE TABLE launch_waits_rebuilt (
    invocation_id   TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id          TEXT CHECK (run_id IS NULL OR run_id <> ''),
    task_created_at TEXT CHECK (task_created_at IS NULL OR task_created_at <> ''),
    waiting_since   TEXT NOT NULL CHECK (waiting_since <> ''),
    refused_at      TEXT NOT NULL CHECK (refused_at <> ''),
    kept_at         TEXT CHECK (kept_at IS NULL OR kept_at <> ''),
    CHECK ((run_id IS NULL) = (task_created_at IS NULL))
) STRICT;

INSERT INTO launch_waits_rebuilt
    (invocation_id, run_id, task_created_at, waiting_since, refused_at, kept_at)
SELECT invocation_id, run_id, task_created_at, waiting_since, refused_at, kept_at
FROM launch_waits;

DROP TABLE launch_waits;
ALTER TABLE launch_waits_rebuilt RENAME TO launch_waits;
