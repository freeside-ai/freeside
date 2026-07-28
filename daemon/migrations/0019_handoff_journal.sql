-- Production handoff recovery and the identity-to-volume trust binding
-- (issues #373, #370, and #372).
--
-- Existing auth identities receive NULL, deliberately. No migration can infer
-- which ambient runtime volume belongs to an identity, and guessing would
-- recreate the cross-identity writable-store bug this column closes. Current
-- binaries reject a lease-declaring identity whose binding is empty, so a
-- legacy leased identity and every lease row behind it fail closed until the
-- identity is explicitly re-enrolled under trusted configuration.
ALTER TABLE auth_identities ADD COLUMN auth_store_volume TEXT;

-- One durable row per run, ever. Extracted columns are cross-checked against
-- the JSON body on reconstruction; the body keeps the store's persistence
-- shape self-contained while the columns enforce lookup and state-machine
-- constraints. Lease fields are all-present or all-absent.
CREATE TABLE handoff_journal_records (
    run_id                    TEXT NOT NULL PRIMARY KEY CHECK (run_id <> ''),
    ownership_token           TEXT NOT NULL CHECK (ownership_token <> ''),
    spec_digest               TEXT NOT NULL CHECK (spec_digest <> ''),
    observed_base_sha         TEXT NOT NULL,
    credential_pre_digest     TEXT NOT NULL,
    writer_complete           INTEGER NOT NULL CHECK (writer_complete IN (0, 1)),
    lease_auth_identity_id    TEXT REFERENCES auth_identities (id),
    lease_holder              TEXT,
    lease_fence               INTEGER,
    lease_acquired_at         TEXT,
    lease_expires_at          TEXT,
    export_dir                TEXT NOT NULL,
    outcome                   TEXT CHECK (outcome IS NULL OR outcome IN ('completed', 'loss')),
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
