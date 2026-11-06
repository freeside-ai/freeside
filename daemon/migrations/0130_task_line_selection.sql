-- Task lines carry a selection (issue #2597): a line chooses an enrollment,
-- a model, and an effort instead of naming a tree agent. A line is content
-- addressed and admissions cite it by id, so a stored line is never
-- rewritten: the selection is a second encoding (domain.TaskLineEncoding,
-- freeside.task_line/v2) beside the name, and every existing row stays v1
-- with its bytes and id. #2599 converts the v1 rows.
--
-- encoding names the row's encoding. A v1 row names its agent in agent; a v2
-- row holds enrollment_id, model_id, and effort, all three or none; a cleared
-- row of either encoding holds neither. 0125's CHECK ties a clear to a NULL
-- agent alone, and SQLite cannot change a CHECK in place, so rebuild the
-- table in one migrator-owned transaction. Every other column and constraint
-- of 0125 is carried, and every row is copied unchanged as v1. No table
-- references task_lines. task_line_changes stores each change's JSON body,
-- which names its own encoding, so it needs no change.
ALTER TABLE task_lines RENAME TO task_lines_v3;

CREATE TABLE task_lines (
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    role           TEXT    NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
    version        INTEGER NOT NULL CHECK (version > 0),
    id             TEXT    NOT NULL UNIQUE CHECK (id <> ''),
    encoding       TEXT    NOT NULL CHECK (encoding IN ('freeside.task_line/v1', 'freeside.task_line/v2')),
    agent          TEXT    CHECK (agent <> ''),
    enrollment_id  TEXT    CHECK (enrollment_id <> ''),
    model_id       TEXT    CHECK (model_id <> ''),
    effort         TEXT    CHECK (effort <> ''),
    predecessor_id TEXT,
    source         TEXT    NOT NULL CHECK (source <> ''),
    set_by         TEXT    NOT NULL CHECK (set_by <> ''),
    set_at         TEXT    NOT NULL,
    provenance     TEXT    CHECK ((source IN ('router', 'alternate_agent_move')) = (provenance IS NOT NULL)),
    cleared        INTEGER NOT NULL DEFAULT 0 CHECK (cleared IN (0, 1)),
    CHECK ((version = 1) = (predecessor_id IS NULL)),
    CHECK ((enrollment_id IS NULL) = (model_id IS NULL) AND (model_id IS NULL) = (effort IS NULL)),
    CHECK (encoding = 'freeside.task_line/v2' OR enrollment_id IS NULL),
    CHECK (encoding = 'freeside.task_line/v1' OR agent IS NULL),
    CHECK ((cleared = 1) = (agent IS NULL AND enrollment_id IS NULL)),
    PRIMARY KEY (task_id, role, version)
) STRICT;

INSERT INTO task_lines
    (task_id, role, version, id, encoding, agent, predecessor_id, source, set_by, set_at, provenance, cleared)
SELECT task_id, role, version, id, 'freeside.task_line/v1', agent, predecessor_id, source, set_by, set_at,
    provenance, cleared
FROM task_lines_v3;

DROP TABLE task_lines_v3;
