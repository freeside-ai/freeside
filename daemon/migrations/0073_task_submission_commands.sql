-- TaskSubmission: the durable, immutable record of one accepted submit_task
-- ClientCommand (domain.TaskSubmission, plan §5.11, §5.14). Unlike a decision
-- command it binds to no attention item: it names the task the command created
-- or fetched by the project-scoped intake key and that task's specification
-- run. Write-once, keyed by the client-generated command_id: a retry converges
-- on the stored row and a changed body under that id is a conflict
-- (effectively-once, §5.9). The command_id is unique across command kinds -- the
-- acceptance boundary probes the decision commands table for the same id, and
-- the reverse -- so one id never names both a decision and a submission. The
-- extracted columns are cross-checked against the body on read; the committed
-- result is as_of_revision, the client-visible revision the command applied at.
-- The existing commands table is unchanged: its item_id column is NOT NULL and
-- references attention_items, which a task submission has none of.

CREATE TABLE task_submission_commands (
    command_id           TEXT PRIMARY KEY,
    device_id            TEXT    NOT NULL,
    project_id           TEXT    NOT NULL,
    task_id              TEXT    NOT NULL REFERENCES tasks (id),
    specification_run_id TEXT    NOT NULL REFERENCES runs (id),
    entity_version       INTEGER NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL
) STRICT;
