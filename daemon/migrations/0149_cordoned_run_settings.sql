-- Cordoned run settings (issue #3471).
--
-- task_writer_classes holds a task's requirement that its writers run
-- warded. A task with no row has none, and its writers take the project's
-- class. domain.TaskWriterClass.Validate accepts only the warded class, on
-- write and on decode, so the column carries no CHECK list
-- (devlog/2026-11-07-1113-enum-columns-go-validation.md).
--
-- launch_waits gains resource, the order a wait is in: memory, the
-- order under the memory budget, or cordoned_launch, the order under the
-- cordoned launch cap. Each gate counts only its own waiters. Every
-- existing row is a memory wait, which the default records. The store's
-- LaunchWaitResource.valid() guards the column on write and decode, so it
-- carries no CHECK list either.
--
-- A plain create and an added column, with no rebuild. Both tables are
-- daemon-internal: never synchronized.
CREATE TABLE task_writer_classes (
    task_id TEXT NOT NULL PRIMARY KEY REFERENCES tasks (id),
    class   TEXT NOT NULL CHECK (class <> ''),
    set_by  TEXT NOT NULL CHECK (set_by <> ''),
    set_at  TEXT NOT NULL CHECK (set_at <> '')
) STRICT;

ALTER TABLE launch_waits ADD COLUMN resource TEXT NOT NULL DEFAULT 'memory' CHECK (resource <> '');
