-- Shared read holds on an identity's auth store (issue #1585). A read hold
-- lets several executions read one store through read-only mounts at once;
-- the mutation lease stays exclusive, and the store refuses each while the
-- other is live. One row per identity and holder: a holder is one run's
-- invocation, so a later run always opens its own row.
CREATE TABLE auth_store_read_holds (
    auth_identity_id     TEXT    NOT NULL REFERENCES auth_identities (id),
    holder               TEXT    NOT NULL CHECK (holder <> ''),
    acquired_at          TEXT    NOT NULL CHECK (acquired_at <> ''),
    expires_at           TEXT    NOT NULL CHECK (expires_at <> ''),
    expires_at_unix_nano INTEGER NOT NULL,
    released_at          TEXT,
    body                 TEXT    NOT NULL,
    PRIMARY KEY (auth_identity_id, holder)
) STRICT;

-- The mutation lease asks "is any read hold on this identity live?". Released
-- rows drop out of the index; expiry is compared against the caller's clock.
CREATE INDEX auth_store_read_holds_unreleased
    ON auth_store_read_holds (auth_identity_id, expires_at_unix_nano)
    WHERE released_at IS NULL;

-- A journal record references either the exclusive lease window or a read
-- hold window it opened, never both. SQLite cannot add a table CHECK in
-- place, so rebuild the table as 0022 did, preserving every durable row.
ALTER TABLE handoff_journal_records RENAME TO handoff_journal_records_v2;

CREATE TABLE handoff_journal_records (
    run_id                    TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    ownership_token           TEXT NOT NULL CHECK (ownership_token <> ''),
    spec_digest               TEXT NOT NULL CHECK (spec_digest <> ''),
    observed_base_sha         TEXT NOT NULL,
    credential_pre_digest     TEXT NOT NULL,
    writer_complete           INTEGER NOT NULL CHECK (writer_complete IN (0, 1)),
    cancellation_requested    INTEGER NOT NULL CHECK (cancellation_requested IN (0, 1)),
    writer_failure_status     INTEGER CHECK (writer_failure_status BETWEEN 1 AND 255),
    state_preparation         TEXT NOT NULL,
    instruction_preparation   TEXT NOT NULL,
    lease_auth_identity_id    TEXT REFERENCES auth_identities (id),
    lease_holder              TEXT,
    lease_fence               INTEGER,
    lease_acquired_at         TEXT,
    lease_expires_at          TEXT,
    read_hold_auth_identity_id TEXT REFERENCES auth_identities (id),
    read_hold_holder          TEXT,
    read_hold_acquired_at     TEXT,
    read_hold_expires_at      TEXT,
    export_dir                TEXT NOT NULL,
    outcome                   TEXT CHECK (outcome IS NULL OR outcome IN ('completed', 'canceled', 'failed', 'loss')),
    opened_at                 TEXT NOT NULL CHECK (opened_at <> ''),
    body                      TEXT NOT NULL,
    CHECK (
        (lease_auth_identity_id IS NULL AND lease_holder IS NULL AND
         lease_fence IS NULL AND lease_acquired_at IS NULL AND lease_expires_at IS NULL)
        OR
        (lease_auth_identity_id IS NOT NULL AND lease_holder IS NOT NULL AND
         lease_fence > 0 AND lease_acquired_at IS NOT NULL AND lease_expires_at IS NOT NULL)
    ),
    CHECK (
        (read_hold_auth_identity_id IS NULL AND read_hold_holder IS NULL AND
         read_hold_acquired_at IS NULL AND read_hold_expires_at IS NULL)
        OR
        (read_hold_auth_identity_id IS NOT NULL AND read_hold_holder IS NOT NULL AND
         read_hold_acquired_at IS NOT NULL AND read_hold_expires_at IS NOT NULL)
    ),
    CHECK (lease_auth_identity_id IS NULL OR read_hold_auth_identity_id IS NULL)
) STRICT;

INSERT INTO handoff_journal_records (
    run_id, ownership_token, spec_digest, observed_base_sha,
    credential_pre_digest, writer_complete, cancellation_requested,
    writer_failure_status, state_preparation, instruction_preparation,
    lease_auth_identity_id,
    lease_holder, lease_fence, lease_acquired_at, lease_expires_at,
    export_dir, outcome, opened_at, body
)
SELECT
    run_id, ownership_token, spec_digest, observed_base_sha,
    credential_pre_digest, writer_complete, cancellation_requested,
    writer_failure_status, state_preparation, instruction_preparation,
    lease_auth_identity_id,
    lease_holder, lease_fence, lease_acquired_at, lease_expires_at,
    export_dir, outcome, opened_at, body
FROM handoff_journal_records_v2;

DROP TABLE handoff_journal_records_v2;
