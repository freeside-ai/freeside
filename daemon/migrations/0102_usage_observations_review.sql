-- Usage observations for review invocations (issue #2212). A review is
-- admitted into review_admissions, not execution_admissions, so the foreign
-- key 0061 put on invocation_id refused every review row. The store still
-- attributes each row from an immutable admission of either kind and writes
-- nothing for an invocation absent from both; only SQLite's referential check
-- goes. SQLite cannot drop a foreign key in place, so rebuild the table as
-- 0082 did, preserving every durable row, then restore its indexes and its
-- append-only triggers.
DROP TRIGGER usage_observations_append_only_update;
DROP TRIGGER usage_observations_append_only_delete;
DROP INDEX usage_observations_run;
DROP INDEX usage_observations_treatment;

ALTER TABLE usage_observations RENAME TO usage_observations_v2;

CREATE TABLE usage_observations (
    invocation_id    TEXT    NOT NULL CHECK (invocation_id <> ''),
    run_id           TEXT    NOT NULL REFERENCES runs (id),
    agent_digest     TEXT    NOT NULL CHECK (agent_digest <> ''),
    launch_digest    TEXT    NOT NULL CHECK (launch_digest <> ''),
    treatment_digest TEXT    NOT NULL CHECK (treatment_digest <> ''),
    pricing_revision TEXT    NOT NULL CHECK (pricing_revision <> ''),
    source           TEXT    NOT NULL CHECK (source <> ''),
    kind             TEXT    NOT NULL CHECK (kind <> ''),
    metric           TEXT    NOT NULL CHECK (metric <> ''),
    unit             TEXT    NOT NULL CHECK (unit <> ''),
    quantity         INTEGER NOT NULL CHECK (quantity >= 0),
    sequence         INTEGER NOT NULL CHECK (sequence > 0),
    observed_at      TEXT    NOT NULL CHECK (observed_at <> ''),
    PRIMARY KEY (invocation_id, source, kind, metric, sequence)
) STRICT;

INSERT INTO usage_observations (
    invocation_id, run_id, agent_digest, launch_digest, treatment_digest,
    pricing_revision, source, kind, metric, unit, quantity, sequence,
    observed_at
)
SELECT
    invocation_id, run_id, agent_digest, launch_digest, treatment_digest,
    pricing_revision, source, kind, metric, unit, quantity, sequence,
    observed_at
FROM usage_observations_v2;

DROP TABLE usage_observations_v2;

CREATE INDEX usage_observations_run
    ON usage_observations (run_id);

CREATE INDEX usage_observations_treatment
    ON usage_observations (treatment_digest);

CREATE TRIGGER usage_observations_append_only_update
BEFORE UPDATE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage observations are append-only');
END;

CREATE TRIGGER usage_observations_append_only_delete
BEFORE DELETE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage observations are append-only');
END;
