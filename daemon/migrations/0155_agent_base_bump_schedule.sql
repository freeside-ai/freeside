-- Agent-base bump schedule (issue #2338).
--
-- schedules is rebuilt so kind carries no CHECK list, for the new
-- agent_base_bump kind: Schedule.Validate refuses an unknown kind on write
-- and on decode, so a later kind adds no rebuild
-- (devlog/2026-11-07-1113-enum-columns-go-validation.md). The rebuild copies
-- every row unchanged and keeps the run-binding CHECK, whose kind list names
-- the kinds that bind a run rather than every kind. Nothing reads the table
-- under a foreign key; its one index is recreated.
CREATE TABLE schedules_rebuilt (
    id             TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    project_id     TEXT NOT NULL CHECK (project_id <> ''),
    kind           TEXT NOT NULL CHECK (kind <> ''),
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

INSERT INTO schedules_rebuilt
    (id, project_id, kind, status, generation, run_id, policy_digest, fire_at,
     entity_version, as_of_revision, body)
SELECT id, project_id, kind, status, generation, run_id, policy_digest, fire_at,
       entity_version, as_of_revision, body
FROM schedules;

DROP TABLE schedules;
ALTER TABLE schedules_rebuilt RENAME TO schedules;
CREATE INDEX schedules_due ON schedules (status, fire_at);
