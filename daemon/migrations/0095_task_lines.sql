-- Task lines (plan §5.4 Admitted Agents, revision 73; issue #1600): the
-- operator's per-task choice of agent for one ward role.
--
-- A row is one line version and is never updated. A change appends the next
-- version for the task and role, naming the version it supersedes, so the
-- line an admission cites stays the choice that authorized that attempt. The
-- store assigns version and predecessor_id inside the write transaction; the
-- primary key arbitrates two writers that read the same last version.
--
-- role is checked against the four task-line roles here as well as in the
-- domain type: the shadow reviewer and the wardless roles stay on the lineup,
-- and a row for one would be read by admission (#1640) as a choice nobody
-- may make.
--
-- id is the content address of the row's other columns. It is UNIQUE
-- because an admission cites a line by id alone (selection_record_id).
CREATE TABLE task_lines (
    task_id        TEXT    NOT NULL REFERENCES tasks (id),
    role           TEXT    NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
    version        INTEGER NOT NULL CHECK (version > 0),
    id             TEXT    NOT NULL UNIQUE CHECK (id <> ''),
    agent          TEXT    NOT NULL CHECK (agent <> ''),
    predecessor_id TEXT,
    source         TEXT    NOT NULL CHECK (source <> ''),
    set_by         TEXT    NOT NULL CHECK (set_by <> ''),
    set_at         TEXT    NOT NULL,
    CHECK ((version = 1) = (predecessor_id IS NULL)),
    PRIMARY KEY (task_id, role, version)
) STRICT;
