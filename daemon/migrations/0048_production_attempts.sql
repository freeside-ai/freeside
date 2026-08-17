-- Explicit production-acceptance campaign and attempt lineage. Existing runs
-- remain legacy rows with null lineage columns; new campaign runs cross-check
-- these extracted values against their canonical JSON body on every read.

ALTER TABLE runs ADD COLUMN campaign_id TEXT;
ALTER TABLE runs ADD COLUMN attempt_number INTEGER;
ALTER TABLE runs ADD COLUMN attempt_reason TEXT;
ALTER TABLE runs ADD COLUMN parent_run_id TEXT REFERENCES runs (id);

CREATE TABLE production_attempts (
    campaign_id          TEXT    NOT NULL,
    attempt_number       INTEGER NOT NULL CHECK (attempt_number >= 1),
    kind                 TEXT    NOT NULL CHECK (kind IN ('initial', 'retry')),
    parent_run_id        TEXT,
    source_digest        TEXT    NOT NULL,
    approved_spec_digest TEXT,
    elaboration_run_id   TEXT    NOT NULL,
    implementation_run_id TEXT  NOT NULL UNIQUE,
    reason               TEXT    NOT NULL,
    as_of_revision       INTEGER NOT NULL,
    body                 TEXT    NOT NULL,
    PRIMARY KEY (campaign_id, attempt_number)
) STRICT;
