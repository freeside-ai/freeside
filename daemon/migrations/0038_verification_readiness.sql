-- Section 6 readiness records are immutable evidence, except waiver lifecycle,
-- which is an append-only chain. Read paths re-gate every copied column and
-- current registry generation; these constraints close direct-SQL basics.
CREATE TABLE requirement_resolutions (
    digest                    TEXT PRIMARY KEY CHECK (digest <> ''),
    requirement_key           TEXT NOT NULL CHECK (requirement_key <> ''),
    check_class               TEXT NOT NULL CHECK (check_class IN ('clean_verification', 'independent_review', 'repo_change_policy')),
    requirement_set_digest    TEXT NOT NULL CHECK (requirement_set_digest <> ''),
    floor_registry_generation INTEGER NOT NULL CHECK (floor_registry_generation > 0),
    resolved_policy_digest    TEXT NOT NULL CHECK (resolved_policy_digest <> ''),
    body_digest               TEXT NOT NULL CHECK (body_digest <> ''),
    body                      TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE INDEX requirement_resolutions_by_set_key
    ON requirement_resolutions (requirement_set_digest, requirement_key);

CREATE TABLE check_proofs (
    digest                        TEXT PRIMARY KEY CHECK (digest <> ''),
    requirement_resolution_digest TEXT NOT NULL REFERENCES requirement_resolutions (digest),
    candidate_head                TEXT NOT NULL CHECK (candidate_head <> ''),
    recipe_digest                 TEXT NOT NULL CHECK (recipe_digest <> ''),
    body_digest                   TEXT NOT NULL CHECK (body_digest <> ''),
    body                          TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE degraded_waivers (
    waiver_id                      TEXT PRIMARY KEY CHECK (waiver_id <> ''),
    requirement_resolution_digest TEXT NOT NULL REFERENCES requirement_resolutions (digest),
    check_class                    TEXT NOT NULL CHECK (check_class = 'repo_change_policy'),
    authority                      TEXT NOT NULL CHECK (authority IN ('explicit_human_approval', 'daemon_trusted_configuration')),
    floor_registry_generation      INTEGER NOT NULL CHECK (floor_registry_generation > 0),
    lifecycle_digest               TEXT NOT NULL CHECK (lifecycle_digest <> ''),
    body_digest                    TEXT NOT NULL CHECK (body_digest <> ''),
    body                           TEXT NOT NULL CHECK (body <> '')
) STRICT;

CREATE TABLE waiver_lifecycle_events (
    waiver_id       TEXT NOT NULL REFERENCES degraded_waivers (waiver_id),
    sequence        INTEGER NOT NULL CHECK (sequence > 0),
    status          TEXT NOT NULL CHECK (status IN ('granted', 'revoked', 'expired')),
    previous_digest TEXT NOT NULL,
    event_digest    TEXT NOT NULL UNIQUE CHECK (event_digest <> ''),
    recorded_at     TEXT NOT NULL CHECK (recorded_at <> ''),
    body_digest     TEXT NOT NULL CHECK (body_digest <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (waiver_id, sequence),
    CHECK ((sequence = 1 AND status = 'granted' AND previous_digest = '') OR
           (sequence > 1 AND previous_digest <> ''))
) STRICT;
