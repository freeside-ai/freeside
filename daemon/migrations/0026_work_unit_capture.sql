-- Work-unit capture records (plan §5.18, issue #443): the explicit
-- declarations and first-party observations the 1B.2 frontier projection
-- will derive from. Daemon-internal rows, never synchronized client state,
-- so no entity_version or as_of_revision (the 0014 rule): the projection —
-- not the raw records — is the client surface, and it ships in 1B.2 with
-- its own contract. Unlike the 0024 observation projection these rows are
-- the authority later derivation stands on, so readers run the full store
-- reconstruction gate (decode, re-validate, cross-check the extracted
-- columns) and fail closed.
--
-- No backfill: a run submitted before this migration legitimately has no
-- declaration, and inventing one would present reconstruction as an
-- operator's explicit statement.

-- One declaration per unit and per run, write-once: the operator's
-- statement at submission, verbatim in the body (criterion, bound issue,
-- dependency edges, declared paths, contract serialization). Replays
-- converge byte-identically; a differing re-declaration is a conflict,
-- never an update.
CREATE TABLE work_unit_declarations (
    unit_id     TEXT NOT NULL PRIMARY KEY CHECK (unit_id <> ''),
    run_id      TEXT NOT NULL UNIQUE REFERENCES runs (id),
    project_id  TEXT NOT NULL CHECK (project_id <> ''),
    body        TEXT NOT NULL CHECK (body <> ''),
    declared_at TEXT NOT NULL CHECK (declared_at <> '')
) STRICT;

-- The exact daemon-recorded PR binding (§5.18: a merge marks a unit done
-- only through this binding and the declared criterion), write-once from
-- first-party publication facts. A binding requires its declaration: an
-- undeclared unit has nothing to bind.
CREATE TABLE work_unit_pr_bindings (
    unit_id       TEXT NOT NULL PRIMARY KEY
                      REFERENCES work_unit_declarations (unit_id),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

-- Observed merge-state timeline per pull request, appended on material
-- change only (the domain's MaterialChangeFrom is the single definition),
-- so the history is the resource's state timeline, not a polling log. Keyed
-- by the forge's canonical repository id, per the BaseRevision convention.
CREATE TABLE pull_merge_facts (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    pr_number     INTEGER NOT NULL CHECK (pr_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;

CREATE INDEX pull_merge_facts_resource
    ON pull_merge_facts (repository_id, pr_number, id);

-- Observed issue-state timeline, same append-on-material-change rule.
CREATE TABLE issue_state_facts (
    id            INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    issue_number  INTEGER NOT NULL CHECK (issue_number > 0),
    body          TEXT NOT NULL CHECK (body <> ''),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;

CREATE INDEX issue_state_facts_resource
    ON issue_state_facts (repository_id, issue_number, id);

-- The write-once completion record: the declared criterion was exactly
-- satisfied. Partial, stacked, and related merges never reach this table;
-- the domain evaluator is the only constructor.
CREATE TABLE work_unit_completions (
    unit_id     TEXT NOT NULL PRIMARY KEY
                    REFERENCES work_unit_declarations (unit_id),
    body        TEXT NOT NULL CHECK (body <> ''),
    recorded_at TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;
