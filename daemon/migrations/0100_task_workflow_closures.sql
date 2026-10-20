-- Administrative workflow closure is independent of slot release and Stop.
-- Existing abandonment facts do not identify their caller and grant no closure.
CREATE TABLE task_workflow_closures (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    start_ordinal INTEGER NOT NULL CHECK (start_ordinal > 0),
    start_run_id TEXT NOT NULL REFERENCES runs(id),
    revision INTEGER NOT NULL CHECK (revision > 0),
    body TEXT NOT NULL,
    PRIMARY KEY (task_id, start_ordinal),
    FOREIGN KEY (task_id, start_ordinal) REFERENCES task_lifecycle_facts(task_id, ordinal)
) STRICT;

-- A task-subject notice deliberately has no public run_id. Retain its producer
-- privately so a delayed write or cleanup cannot acquire a newer episode.
CREATE TABLE task_attention_workflow_origins (
    item_id TEXT PRIMARY KEY REFERENCES attention_items(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    run_id TEXT NOT NULL REFERENCES runs(id),
    body TEXT NOT NULL
) STRICT;
