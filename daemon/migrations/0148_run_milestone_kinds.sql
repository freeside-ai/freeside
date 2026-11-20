-- Verification milestones (issue #3446). A run now records
-- verification_started, verification_passed, and verification_failed on its
-- verification invocation, so its timeline and clocks show the run verifying.
--
-- run_milestones is rebuilt so kind carries no CHECK list:
-- RunMilestone.Validate refuses an unknown kind on write and on decode, so a
-- later kind adds no rebuild (devlog/2026-11-07-1113-enum-columns-go-validation.md).
--
-- The rebuild copies every row unchanged, ids included, so record order (the
-- id) is kept, and recreates the unique identity index that makes
-- AppendRunMilestone first observation wins. Nothing reads the table under a
-- foreign key.

CREATE TABLE run_milestones_rebuilt (
    id            INTEGER PRIMARY KEY,
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    kind          TEXT NOT NULL CHECK (kind <> ''),
    invocation_id TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    terminal      TEXT CHECK (terminal IS NULL OR terminal <> ''),
    outcome       TEXT CHECK (outcome IS NULL OR outcome <> ''),
    reason        TEXT CHECK (reason IS NULL OR reason <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

INSERT INTO run_milestones_rebuilt
    (id, run_id, kind, invocation_id, terminal, outcome, reason, recorded_at)
SELECT id, run_id, kind, invocation_id, terminal, outcome, reason, recorded_at
FROM run_milestones;

DROP TABLE run_milestones;
ALTER TABLE run_milestones_rebuilt RENAME TO run_milestones;
CREATE UNIQUE INDEX run_milestones_identity
    ON run_milestones (run_id, kind, COALESCE(invocation_id, ''));
