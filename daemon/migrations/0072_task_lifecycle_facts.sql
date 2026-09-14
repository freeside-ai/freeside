-- Task lifecycle facts are the append-only log WIP admission counting rests on
-- (issue #1318): one row per recorded start, completion, or abandonment,
-- ordered by ordinal within a task. Idempotent replay dedupes on
-- (task_id, source_id); a completion carries its work-unit binding, and every
-- fact carries the producing run and that run's campaign so the derived
-- current-campaign rule can decide a completion's currency without a lookup.
CREATE TABLE task_lifecycle_facts (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    kind TEXT NOT NULL CHECK (kind IN ('started', 'completed', 'abandoned')),
    run_id TEXT NOT NULL,
    campaign_id TEXT,
    binding_unit_id TEXT,
    source_id TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (task_id, ordinal),
    UNIQUE (task_id, source_id)
) STRICT;
