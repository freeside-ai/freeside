-- Append-only, numbers-only usage observations (plan §8, issue #901). Each
-- row is attributed from the immutable execution admission, never from the
-- caller. Missing rows mean "not observed"; an observed zero stays a row.
CREATE TABLE usage_observations (
    invocation_id    TEXT    NOT NULL REFERENCES execution_admissions (invocation_id),
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
