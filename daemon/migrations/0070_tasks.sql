-- Task identity is independent of the content-addressed run/campaign IDs.
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    entity_version INTEGER NOT NULL CHECK (entity_version >= 1),
    as_of_revision INTEGER NOT NULL CHECK (as_of_revision >= 1),
    body TEXT NOT NULL
) STRICT;

CREATE TABLE task_intake_keys (
    project_id TEXT NOT NULL,
    intake_key TEXT NOT NULL,
    task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) DEFERRABLE INITIALLY DEFERRED,
    PRIMARY KEY (project_id, intake_key)
) STRICT;

-- SQLite needs a default while adding a NOT NULL column to populated runs.
-- The data migration fills it; the insert/update gates below reject that
-- transitional default and enforce the same-project task reference.
ALTER TABLE runs ADD COLUMN task_id TEXT NOT NULL DEFAULT '';
ALTER TABLE attention_items ADD COLUMN subject_task_id TEXT REFERENCES tasks(id);
CREATE INDEX runs_task ON runs(task_id);

CREATE TABLE task_runs (
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(id),
    PRIMARY KEY (task_id, ordinal)
) STRICT;

CREATE TRIGGER runs_task_insert BEFORE INSERT ON runs
WHEN NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN SELECT RAISE(ABORT, 'run task must exist in its project'); END;

CREATE TRIGGER runs_task_update BEFORE UPDATE OF task_id, project_id ON runs
WHEN NOT EXISTS (SELECT 1 FROM tasks WHERE id = NEW.task_id AND project_id = NEW.project_id)
BEGIN SELECT RAISE(ABORT, 'run task must exist in its project'); END;
