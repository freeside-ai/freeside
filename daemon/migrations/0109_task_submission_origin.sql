-- Add the submission origin (#2332): project-image upkeep submits a
-- maintenance task through the ordinary submission primitive with a daemon
-- origin and the refusal record as its source (plan §5.7, Project-image
-- upkeep; §5.12). A daemon-origin row has no device, so device_id becomes
-- nullable, and the CHECK ties the two: a device row names its device and a
-- daemon row names none. The store re-reads the image_upkeep_refusals row a
-- daemon-origin submission names on every write and decode.
--
-- SQLite cannot relax NOT NULL or add a table CHECK in place, so rebuild the
-- table in one migrator-owned transaction. Every existing row is a device
-- submission; its body is copied verbatim, and the store reads a body with
-- no origin as device. No table references task_submission_commands.
ALTER TABLE task_submission_commands RENAME TO task_submission_commands_v1;

CREATE TABLE task_submission_commands (
    command_id           TEXT PRIMARY KEY,
    origin               TEXT    NOT NULL CHECK (origin IN ('device', 'daemon')),
    device_id            TEXT,
    project_id           TEXT    NOT NULL,
    task_id              TEXT    NOT NULL REFERENCES tasks (id),
    specification_run_id TEXT    NOT NULL REFERENCES runs (id),
    entity_version       INTEGER NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL,
    request_digest       TEXT,
    CHECK ((origin = 'device') = (device_id IS NOT NULL))
) STRICT;

INSERT INTO task_submission_commands
    (command_id, origin, device_id, project_id, task_id, specification_run_id,
     entity_version, as_of_revision, body, request_digest)
SELECT command_id, 'device', device_id, project_id, task_id, specification_run_id,
       entity_version, as_of_revision, body, request_digest
FROM task_submission_commands_v1;

DROP TABLE task_submission_commands_v1;
