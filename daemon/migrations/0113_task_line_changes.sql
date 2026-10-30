-- Task line changes (plan §5.4, Admitted Agents; issue #1601): an operator
-- changes or clears a task's line for a role after the task exists.
--
-- task_line_changes holds the record a cli_line_change line cites as its
-- set_by. body is the domain.TaskLineChange JSON, and id its content address;
-- the store re-derives the id from the decoded value on every read, and
-- task_id, role, and recorded_at are copied columns it checks against the
-- body. A change stores the agent it chose, so admission can hold the line to
-- it.
--
-- A cleared version names no agent and takes the role back to the lineup, so
-- agent becomes nullable and cleared records the choice, with the CHECK tying
-- the two: a sentinel name could collide with an agent's. cleared is left out
-- of a line's canonical encoding when 0, so every existing row keeps its id.
--
-- SQLite cannot relax NOT NULL in place, so rebuild the table in one
-- migrator-owned transaction. Every column and constraint of 0095 and 0111
-- is carried, and every row is copied with cleared 0. No table references
-- task_lines.
ALTER TABLE task_lines RENAME TO task_lines_v1;

CREATE TABLE task_lines (
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    role           TEXT    NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
    version        INTEGER NOT NULL CHECK (version > 0),
    id             TEXT    NOT NULL UNIQUE CHECK (id <> ''),
    agent          TEXT    CHECK (agent <> ''),
    predecessor_id TEXT,
    source         TEXT    NOT NULL CHECK (source <> ''),
    set_by         TEXT    NOT NULL CHECK (set_by <> ''),
    set_at         TEXT    NOT NULL,
    provenance     TEXT    CHECK ((source = 'router') = (provenance IS NOT NULL)),
    cleared        INTEGER NOT NULL DEFAULT 0 CHECK (cleared IN (0, 1)),
    CHECK ((version = 1) = (predecessor_id IS NULL)),
    CHECK ((cleared = 1) = (agent IS NULL)),
    PRIMARY KEY (task_id, role, version)
) STRICT;

INSERT INTO task_lines
    (task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance)
SELECT task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance
FROM task_lines_v1;

DROP TABLE task_lines_v1;

CREATE TABLE task_line_changes (
    id          TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    role        TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    body        TEXT NOT NULL
) STRICT;
