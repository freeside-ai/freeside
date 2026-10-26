-- Widen the schedule kind vocabulary with image_upkeep (#2332): the permanent
-- trusted-config project-image upkeep job (plan §5.7, Project-image upkeep).
-- Like doctor and janitor it binds no run or policy, so the authority CHECK
-- from 0027 holds unchanged. SQLite cannot alter a CHECK constraint in
-- place, so rebuild the table in one migrator-owned transaction, copying
-- every row verbatim (no body or revision changes: no row's meaning moves),
-- and recreate the due index. No table references schedules.
ALTER TABLE schedules RENAME TO schedules_v1;
DROP INDEX IF EXISTS schedules_due;

CREATE TABLE schedules (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    project_id     TEXT NOT NULL CHECK (project_id <> ''),
    kind           TEXT NOT NULL CHECK (kind IN (
                       'pr_checks_deadline', 'review_wait_threshold',
                       'base_advance_watch', 'installation_poll',
                       'doctor', 'janitor', 'image_upkeep')),
    status         TEXT NOT NULL CHECK (status IN (
                       'armed', 'fired', 'resolved', 'expired')),
    generation     INTEGER NOT NULL CHECK (generation >= 1),
    run_id         TEXT REFERENCES runs(id),
    policy_digest  TEXT CHECK (policy_digest IS NULL OR policy_digest <> ''),
    fire_at        INTEGER,
    entity_version INTEGER NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body           TEXT NOT NULL,
    CHECK (
        (kind IN ('pr_checks_deadline', 'review_wait_threshold', 'base_advance_watch'))
        = (run_id IS NOT NULL AND policy_digest IS NOT NULL)
    )
) STRICT;

INSERT INTO schedules
    (id, project_id, kind, status, generation, run_id, policy_digest,
     fire_at, entity_version, as_of_revision, body)
SELECT id, project_id, kind, status, generation, run_id, policy_digest,
       fire_at, entity_version, as_of_revision, body
FROM schedules_v1;

DROP TABLE schedules_v1;

CREATE INDEX schedules_due ON schedules (status, fire_at);
