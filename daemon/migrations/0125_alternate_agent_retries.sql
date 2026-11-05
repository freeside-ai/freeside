-- Alternate-agent retry (plan §4, §5.4; issue #869): an operator retries a
-- role under another agent after a quota, credential, or capacity failure,
-- from the failure's card or by moving a task parked on an exhausted quota
-- window.
--
-- alternate_agent_retries holds the card record an attempt or a moved line
-- cites. body is the domain.AlternateAgentRetry JSON, and id its content
-- address; the store re-derives the id from the decoded value on every read,
-- and task_id, command_id, and recorded_at are copied columns it checks
-- against the body. One command makes at most one choice, so command_id is
-- unique.
--
-- run_hold_observations.quota_alternate_agents is the JSON list of
-- domain.AlternateAgentOffer a parked task's hold offers, NULL when it offers
-- none. Each offer is a tree name, a pool, a code, or a model id; never
-- provider text.
--
-- A moved line cites the pool it moved to, as a router line does, so it
-- carries a provenance. 0113's task_lines ties a provenance to the router
-- source alone, and SQLite cannot change a CHECK in place, so rebuild the
-- table in one migrator-owned transaction with that CHECK widened to the
-- move's source. Every other column and constraint of 0113 is carried, and
-- every row is copied unchanged. No table references task_lines.
ALTER TABLE task_lines RENAME TO task_lines_v2;

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
    provenance     TEXT    CHECK ((source IN ('router', 'alternate_agent_move')) = (provenance IS NOT NULL)),
    cleared        INTEGER NOT NULL DEFAULT 0 CHECK (cleared IN (0, 1)),
    CHECK ((version = 1) = (predecessor_id IS NULL)),
    CHECK ((cleared = 1) = (agent IS NULL)),
    PRIMARY KEY (task_id, role, version)
) STRICT;

INSERT INTO task_lines
    (task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance, cleared)
SELECT task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance, cleared
FROM task_lines_v2;

DROP TABLE task_lines_v2;

CREATE TABLE alternate_agent_retries (
    id          TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    task_id     TEXT NOT NULL REFERENCES tasks (id),
    command_id  TEXT NOT NULL UNIQUE CHECK (command_id <> ''),
    recorded_at TEXT NOT NULL,
    body        TEXT NOT NULL
) STRICT;

ALTER TABLE run_hold_observations ADD COLUMN quota_alternate_agents TEXT
    CHECK (quota_alternate_agents IS NULL OR quota_alternate_agents <> '');
