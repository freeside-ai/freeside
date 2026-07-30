-- Add the failed writer outcome without rewriting the digest-pinned 0019
-- migration. SQLite cannot alter a CHECK constraint in place, so rebuild the
-- table in one migrator-owned transaction while preserving every durable row.
ALTER TABLE handoff_journal_records RENAME TO handoff_journal_records_v1;

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
    )
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
    credential_pre_digest, writer_complete, 0,
    NULL, '', '',
    lease_auth_identity_id,
    lease_holder, lease_fence, lease_acquired_at, lease_expires_at,
    export_dir, outcome, opened_at, body
FROM handoff_journal_records_v1;

DROP TABLE handoff_journal_records_v1;
