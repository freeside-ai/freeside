-- Widen the run milestone vocabulary with work_unit_completed (#1134): the
-- run's work unit satisfied its declared completion criterion. The milestone
-- is invocation-only, carrying the publication invocation like
-- publication_ready; the PR, merge commit, and bound issue stay on the
-- re-gated work_unit_completions row, the single authority for the fact.
-- SQLite cannot alter a CHECK constraint in place, so rebuild the table in
-- one migrator-owned transaction while preserving every durable row and the
-- first-observation-wins identity index from 0024.
ALTER TABLE run_milestones RENAME TO run_milestones_v1;
DROP INDEX IF EXISTS run_milestones_identity;

CREATE TABLE run_milestones (
    id            INTEGER PRIMARY KEY,
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    kind          TEXT NOT NULL CHECK (kind IN (
                      'run_submitted', 'invocation_admitted',
                      'invocation_started', 'execution_export_recorded',
                      'execution_outcome_recorded', 'terminal_recorded',
                      'publication_ready', 'publication_blocked',
                      'work_unit_completed')),
    invocation_id TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    terminal      TEXT CHECK (terminal IS NULL OR terminal <> ''),
    outcome       TEXT CHECK (outcome IS NULL OR outcome <> ''),
    reason        TEXT CHECK (reason IS NULL OR reason <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

INSERT INTO run_milestones (
    id, run_id, kind, invocation_id, terminal, outcome, reason, recorded_at
)
SELECT id, run_id, kind, invocation_id, terminal, outcome, reason, recorded_at
FROM run_milestones_v1;

DROP TABLE run_milestones_v1;

CREATE UNIQUE INDEX run_milestones_identity
    ON run_milestones (run_id, kind, COALESCE(invocation_id, ''));

-- The runs list asks every run for its retrying attempt (#1134), so the
-- lineage column gains the index the per-run lookup needs.
CREATE INDEX runs_by_parent ON runs (parent_run_id);
