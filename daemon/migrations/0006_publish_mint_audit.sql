-- Publish mint-audit records (plan §5.9: SQLite owns audit; §8: typed
-- relational rows, no map fields; issue #107). One row per successful
-- installation-token mint, insert-only: two identical mints are two real
-- events, so there is no idempotency key and no dedup. Like the
-- inbox/outbox and device_credentials tables, audit is daemon-internal
-- bookkeeping, not synchronized state, so it carries no
-- entity_version/as_of_revision columns.
--
-- The shape is #80's MintRecord contract: no token column is present, so
-- no audit read path can leak token material. Permission-scope columns
-- are NOT NULL but may be empty: an empty scope means "not requested",
-- and the audit surface records what happened rather than re-policing
-- it. installation_id and repo reference GitHub-side identifiers, not
-- store entities, so no foreign keys apply. Timestamps are RFC3339Nano
-- UTC written by Go; the store never relies on SQLite clock functions.

CREATE TABLE publish_mint_audits (
    id                      INTEGER PRIMARY KEY,
    minted_at               TEXT    NOT NULL CHECK (minted_at <> ''),
    installation_id         INTEGER NOT NULL CHECK (installation_id > 0),
    repo                    TEXT    NOT NULL CHECK (repo <> ''),
    requested_contents      TEXT    NOT NULL,
    requested_pull_requests TEXT    NOT NULL,
    requested_metadata      TEXT    NOT NULL,
    granted_contents        TEXT    NOT NULL,
    granted_pull_requests   TEXT    NOT NULL,
    granted_metadata        TEXT    NOT NULL,
    expires_at              TEXT    NOT NULL CHECK (expires_at <> '')
) STRICT;
