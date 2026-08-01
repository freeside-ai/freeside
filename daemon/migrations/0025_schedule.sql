-- Durable scheduler state (plan §5.16, issue #442), split by visibility:
--
--   * schedules is the synchronized aggregate (entity_version /
--     as_of_revision): what is scheduled, its binding, and how it ended.
--     The kind and status CHECKs mirror domain.AllScheduleKinds and
--     domain.AllScheduleStatuses; widening either vocabulary is a
--     kind:contract change and lands with its own migration.
--   * schedule_timers and schedule_occurrences are daemon-internal
--     bookkeeping (the 0014 rule: no entity_version or as_of_revision).
--     Syncing the tick clock would bump the §5.14 revision on every
--     recurring fire (the janitor runs every 30 seconds) to tell clients
--     nothing meaningful changed; the client-visible facts stay on the
--     aggregate.
--
-- Instants that order or identify fires (fire_at, next_nominal_fire_at,
-- nominal_fire_at, gap_earliest) are INTEGER UTC unix nanoseconds: the due
-- scan compares them in SQL, and RFC3339Nano's variable-width fraction does
-- not order lexicographically. Audit instants nothing compares (created_at,
-- consumed_at) stay RFC3339Nano text like the outbox's.
--
-- No backfill: pre-scheduler deferred checks ran on plain tickers with no
-- durable identity, and inventing rows for them would present
-- reconstruction as observation.

CREATE TABLE schedules (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    project_id     TEXT NOT NULL CHECK (project_id <> ''),
    kind           TEXT NOT NULL CHECK (kind IN (
                       'pr_checks_deadline', 'review_wait_threshold',
                       'base_advance_watch', 'installation_poll',
                       'doctor', 'janitor')),
    status         TEXT NOT NULL CHECK (status IN (
                       'armed', 'fired', 'resolved', 'expired')),
    generation     INTEGER NOT NULL CHECK (generation >= 1),
    -- One-shot kinds extract their nominal deadline for the due scan;
    -- recurring kinds keep their rolling clock in schedule_timers and leave
    -- this NULL. Kind-scoped presence is domain validation's to enforce.
    fire_at        INTEGER,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT NOT NULL
) STRICT;

CREATE INDEX schedules_due ON schedules (status, fire_at);

-- Rolling next-nominal-fire clock for recurring schedules, one row per
-- schedule, advanced when an occurrence is created and replaced on re-arm.
-- Deleted when the schedule terminates.
CREATE TABLE schedule_timers (
    schedule_id          TEXT NOT NULL PRIMARY KEY CHECK (schedule_id <> ''),
    generation           INTEGER NOT NULL CHECK (generation >= 1),
    next_nominal_fire_at INTEGER NOT NULL
) STRICT;

CREATE INDEX schedule_timers_due ON schedule_timers (next_nominal_fire_at);

-- The occurrence ledger. A pending row is the durably redeliverable fire:
-- consumption and its outcome commit in one transaction or the row stays
-- pending (§5.16). The gap columns record coalesced missed fires; the
-- outcome CHECK stays non-empty-only so the closed vocabulary evolves in
-- one place (domain.AllScheduleOccurrenceOutcomes), while a row an old
-- binary cannot express still fails closed at reconstruction.
CREATE TABLE schedule_occurrences (
    id              INTEGER PRIMARY KEY,
    schedule_id     TEXT NOT NULL CHECK (schedule_id <> ''),
    generation      INTEGER NOT NULL CHECK (generation >= 1),
    nominal_fire_at INTEGER NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'consumed')),
    gap_missed      INTEGER CHECK (gap_missed IS NULL OR gap_missed >= 1),
    gap_earliest    INTEGER,
    created_at      TEXT NOT NULL CHECK (created_at <> ''),
    consumed_at     TEXT CHECK (consumed_at IS NULL OR consumed_at <> ''),
    outcome         TEXT CHECK (outcome IS NULL OR outcome <> ''),
    CHECK ((gap_missed IS NULL) = (gap_earliest IS NULL)),
    CHECK ((status = 'consumed') = (consumed_at IS NOT NULL)),
    CHECK ((status = 'consumed') = (outcome IS NOT NULL))
) STRICT;

-- Occurrence identity (§5.16): replayed fires and crash-retry convergence
-- insert-or-ignore against this key.
CREATE UNIQUE INDEX schedule_occurrences_identity
    ON schedule_occurrences (schedule_id, generation, nominal_fire_at);

CREATE INDEX schedule_occurrences_pending
    ON schedule_occurrences (status, id);
