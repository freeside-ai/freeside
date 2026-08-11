-- Credential-free recovery journal for revoked Codex identities (issue #684).
-- Opening a row shares the transaction that acquires the exact auth-store
-- mutation lease. The immutable header survives a crash; one terminal outcome
-- may later record either a safe failure class or verified replacement
-- coordinates, never credential bytes or provider response text.
CREATE TABLE codex_reenrollment_operations (
    auth_identity_id       TEXT NOT NULL REFERENCES auth_identities (id),
    lease_fence            INTEGER NOT NULL CHECK (lease_fence > 0),
    marker_item_id         TEXT NOT NULL REFERENCES attention_items (id),
    holder                 TEXT NOT NULL CHECK (holder <> ''),
    opened_at              TEXT NOT NULL CHECK (opened_at <> ''),
    outcome                TEXT CHECK (outcome IS NULL OR outcome IN ('failed', 'verified')),
    failure_class          TEXT CHECK (failure_class IS NULL OR failure_class IN
                              ('auth_store_replacement_failed', 'verification_failed', 'lease_lost')),
    auth_store_digest      TEXT,
    access_token_expires_at TEXT,
    completed_at           TEXT,
    body                   TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (auth_identity_id, lease_fence),
    CHECK (
        (outcome IS NULL AND failure_class IS NULL AND auth_store_digest IS NULL AND
         access_token_expires_at IS NULL AND completed_at IS NULL)
        OR
        (outcome = 'failed' AND failure_class IS NOT NULL AND auth_store_digest IS NULL AND
         access_token_expires_at IS NULL AND completed_at IS NOT NULL)
        OR
        (outcome = 'verified' AND failure_class IS NULL AND auth_store_digest IS NOT NULL AND
         auth_store_digest <> '' AND access_token_expires_at IS NOT NULL AND completed_at IS NOT NULL)
    )
) STRICT;

CREATE INDEX codex_reenrollment_operations_latest
    ON codex_reenrollment_operations (auth_identity_id, lease_fence DESC);

-- Accepted recovery decisions remain append-only and are re-gated against the
-- command, carrier item, and latest verified operation on every read.
CREATE TABLE codex_reenrollment_recovery_transitions (
    id                      INTEGER PRIMARY KEY,
    auth_identity_id        TEXT NOT NULL REFERENCES auth_identities (id),
    lease_fence             INTEGER NOT NULL CHECK (lease_fence > 0),
    auth_store_digest       TEXT NOT NULL CHECK (auth_store_digest <> ''),
    access_token_expires_at TEXT NOT NULL CHECK (access_token_expires_at <> ''),
    command_id              TEXT REFERENCES commands (command_id)
                                 CHECK (command_id IS NULL OR command_id <> ''),
    reason                  TEXT NOT NULL CHECK (reason <> ''),
    occurred_at             TEXT NOT NULL CHECK (occurred_at <> ''),
    UNIQUE (auth_identity_id, lease_fence),
    FOREIGN KEY (auth_identity_id, lease_fence)
        REFERENCES codex_reenrollment_operations (auth_identity_id, lease_fence)
) STRICT;

CREATE INDEX codex_reenrollment_recovery_transitions_latest
    ON codex_reenrollment_recovery_transitions (auth_identity_id, id DESC);
