-- The usage observation record, one per usage pool (#1715; plan §5.4, usage
-- observation). Not the per-invocation usage_observations table.
--
-- The body holds the whole record: collector, availability answers, the
-- model-to-window map, window readings, and refusal marks. Each body is shaped
-- as store.poolUsageObservationRecord; the reader cross-checks every column
-- against it. Exhausted state is derived from the body, so no column stores
-- it. Nothing is backfilled: the record is new.
CREATE TABLE pool_usage_observations (
    pool                TEXT NOT NULL PRIMARY KEY REFERENCES usage_pools (id),
    collector           TEXT NOT NULL REFERENCES client_enrollments (id),
    last_attempt_status TEXT CHECK (last_attempt_status IS NULL OR last_attempt_status <> ''),
    last_attempt_at     TEXT CHECK ((last_attempt_status IS NULL) = (last_attempt_at IS NULL)),
    recorded_at         TEXT NOT NULL CHECK (recorded_at <> ''),
    body                TEXT NOT NULL CHECK (body <> '')
) STRICT;
