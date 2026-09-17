-- Acceptance, immutable receipts and runtime acknowledgements are separate.
-- No historical task/run status constitutes a cancellation acknowledgement.
CREATE TABLE task_cancellations (
    request_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    target_digest TEXT NOT NULL,
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL,
    UNIQUE(task_id, target_digest)
) STRICT;
CREATE TABLE task_stop_commands (
    command_id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES task_cancellations(request_id),
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL
) STRICT;
CREATE TABLE task_cancellation_acknowledgements (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL REFERENCES task_cancellations(request_id),
    as_of_revision INTEGER NOT NULL,
    body TEXT NOT NULL
) STRICT;
