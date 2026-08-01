-- Run observation: the operator-facing projection of run progress (plan §8,
-- issue #394). Daemon-internal observational rows, never synchronized client
-- state, so no entity_version or as_of_revision (the 0014 rule). They are
-- written inside the same transactions that commit the workflow facts they
-- mirror, but they are never authority: recovery, publication, and teardown
-- re-observe durable records and the runtime (the ward journal's
-- anti-progress-bit position), and readers fully re-validate every row,
-- failing closed on anything the current vocabulary cannot express.
--
-- No backfill: a run submitted before this migration legitimately has no
-- observed timeline, and inventing one would present reconstruction as
-- observation.

-- Append-only milestone timeline, first observation wins. The kind CHECK
-- mirrors domain.AllRunMilestoneKinds; widening the vocabulary is a
-- kind:contract change and lands with its own migration. Detail columns
-- (terminal, outcome, reason) are kind-scoped closed codes cross-checked by
-- domain validation on read and write; they deliberately carry only a
-- non-empty CHECK here so their vocabularies evolve in one place (the
-- domain), while a row an old binary cannot express still fails closed at
-- reconstruction.
CREATE TABLE run_milestones (
    id            INTEGER PRIMARY KEY,
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    kind          TEXT NOT NULL CHECK (kind IN (
                      'run_submitted', 'invocation_admitted',
                      'invocation_started', 'execution_export_recorded',
                      'execution_outcome_recorded', 'terminal_recorded',
                      'publication_ready', 'publication_blocked')),
    invocation_id TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    terminal      TEXT CHECK (terminal IS NULL OR terminal <> ''),
    outcome       TEXT CHECK (outcome IS NULL OR outcome <> ''),
    reason        TEXT CHECK (reason IS NULL OR reason <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
) STRICT;

-- One milestone per (run, kind, invocation): replayed transactions and
-- crash-retry convergence insert-or-ignore against this key, so the first
-- observed instant stands.
CREATE UNIQUE INDEX run_milestones_identity
    ON run_milestones (run_id, kind, COALESCE(invocation_id, ''));

-- Last observation per invocation, last write wins: what the driver
-- reported, whether it currently observed the execution itself, and the
-- daemon-clock instant. History lives in run_milestones; this row only
-- answers "when did the daemon last look, and what did it see", which is
-- what makes a daemon or runtime observation gap derivable after the fact.
CREATE TABLE invocation_observations (
    invocation_id TEXT NOT NULL PRIMARY KEY CHECK (invocation_id <> ''),
    run_id        TEXT NOT NULL CHECK (run_id <> ''),
    status        TEXT NOT NULL CHECK (status <> ''),
    live          INTEGER NOT NULL CHECK (live IN (0, 1)),
    observed_at   TEXT NOT NULL CHECK (observed_at <> '')
) STRICT;

CREATE INDEX invocation_observations_by_run ON invocation_observations (run_id);

-- Current hold per run, one row, latest cause wins: a closed reason code and
-- the observation span of that cause. Forward progress (any milestone
-- append) clears the row; a reason change resets the span. Free-text reasons
-- are unrepresentable by design (the publish.MintRecord leak precedent).
CREATE TABLE run_hold_observations (
    run_id            TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    invocation_id     TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
    reason            TEXT NOT NULL CHECK (reason <> ''),
    first_observed_at TEXT NOT NULL CHECK (first_observed_at <> ''),
    last_observed_at  TEXT NOT NULL CHECK (last_observed_at <> '')
) STRICT;
