-- Cordoned run settings (issue #3471).
--
-- task_writer_classes holds a task's requirement that its writers run
-- warded. A task with no row has none, and its writers take the project's
-- class. domain.TaskWriterClass.Validate accepts only the warded class, on
-- write and on decode, so the column carries no CHECK list
-- (devlog/2026-11-07-1113-enum-columns-go-validation.md).
--
-- A plain create, with no rebuild. The table is daemon-internal: never
-- synchronized.
CREATE TABLE task_writer_classes (
    task_id TEXT NOT NULL PRIMARY KEY REFERENCES tasks (id),
    class   TEXT NOT NULL CHECK (class <> ''),
    set_by  TEXT NOT NULL CHECK (set_by <> ''),
    set_at  TEXT NOT NULL CHECK (set_at <> '')
) STRICT;
