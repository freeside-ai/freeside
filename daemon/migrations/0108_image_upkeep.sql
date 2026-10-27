-- The project-image upkeep records (plan §5.7, Project-image upkeep; issue
-- #2332). Both are daemon-internal runtime state, not synchronized client
-- state, so neither carries an entity_version or as_of_revision.
--
-- image_upkeep_refusals holds the immutable record of one refused upkeep run.
-- id is the content address of every fact but recorded_at, so the same
-- refusal at the same tip converges on the first row. The hold facts, the
-- adoption card, and a daemon-origin task submission reference it by id.
--
-- repository_tip_observations holds the latest default-branch tip read for
-- each repository, independent of any bound PR, and the tip the upkeep job
-- last ran at (NULL until it first runs). The row is mutable.

CREATE TABLE image_upkeep_refusals (
    id            TEXT PRIMARY KEY CHECK (id <> ''),
    repository_id INTEGER NOT NULL CHECK (repository_id > 0),
    harness       TEXT NOT NULL CHECK (harness <> ''),
    tip_sha       TEXT NOT NULL CHECK (length(tip_sha) = 40),
    class         TEXT NOT NULL CHECK (class <> ''),
    recorded_at   TEXT NOT NULL CHECK (recorded_at <> ''),
    body          TEXT NOT NULL
) STRICT;

CREATE INDEX image_upkeep_refusals_repository ON image_upkeep_refusals (repository_id);

CREATE TABLE repository_tip_observations (
    repository_id  INTEGER PRIMARY KEY CHECK (repository_id > 0),
    branch         TEXT NOT NULL CHECK (branch <> ''),
    tip_sha        TEXT NOT NULL CHECK (length(tip_sha) = 40),
    observed_at    TEXT NOT NULL CHECK (observed_at <> ''),
    upkeep_tip_sha TEXT CHECK (upkeep_tip_sha IS NULL OR length(upkeep_tip_sha) = 40),
    body           TEXT NOT NULL
) STRICT;
