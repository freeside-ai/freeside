-- Installation-scope mint-audit records (plan §5.9: SQLite owns audit; §8:
-- typed relational rows, no map fields; issue #545). One row per installation
-- access token the janitor's grant-read mint causes GitHub to create
-- (internal/publish/janitor.go), recorded the moment the token's existence is
-- known -- before the returned grant is validated -- so that no minted token
-- can leave the daemon holding a live, unrevocable credential with no audit
-- row. That is the exact "minted-but-unrevocable" gap the issue closes:
-- revocation is attempted for every minted token, and a token whose revoke
-- also fails is exactly the one that must already be on the ledger.
--
-- The worker mint's publish_mint_audits (0006) is repository-scoped and cannot
-- hold these: an installation-scope mint has no single repository, and 0011's
-- legacy repository_id = 0 sentinel makes a per-scope validity CHECK unwritable
-- without a carve-out. A separate typed table keeps invalid states
-- unrepresentable, so this is an additive CREATE TABLE, never a rebuild of
-- publish_mint_audits, and 0006's rows are never rewritten.
--
-- Insert-only with no idempotency key: two identical mints are two real
-- events. Like publish_mint_audits and the inbox/outbox queues, audit is
-- daemon-internal bookkeeping, not synchronized state, so it carries no
-- entity_version/as_of_revision columns.
--
-- No token column is present, so no audit read path can leak token material
-- (0006's no-leak property). registration_id and installation_id are the
-- strictly positive GitHub coordinates of the credential (a fresh table has no
-- legacy-unknown sentinels to tolerate). outcome records the validation verdict
-- reached after the token was minted: a token whose returned grant matched the
-- request with a valid expiry (the clean path that is then used), or one whose
-- grant was rejected, whose expiry was rejected, or whose response could not be
-- decoded. The requested scopes are always the fixed grant-read request; the
-- granted scopes are populated only when the scope comparison passed, and are
-- otherwise empty because the daemon does not vouch for a grant it rejected.
-- expires_at is nullable for the same reason: a rejected or undecodable mint
-- has no expiry the daemon trusts, and fabricating one would record an instant
-- that was never validated. installation_id references a GitHub-side
-- identifier, not a store entity, so no foreign keys apply. Timestamps are
-- RFC3339Nano UTC written by Go; the store never relies on SQLite clock
-- functions.

CREATE TABLE publish_installation_mint_audits (
    id                       INTEGER PRIMARY KEY,
    minted_at                TEXT    NOT NULL CHECK (minted_at <> ''),
    registration_id          INTEGER NOT NULL CHECK (registration_id > 0),
    installation_id          INTEGER NOT NULL CHECK (installation_id > 0),
    outcome                  TEXT    NOT NULL CHECK (outcome <> ''),
    requested_actions        TEXT    NOT NULL,
    requested_administration TEXT    NOT NULL,
    requested_contents       TEXT    NOT NULL,
    requested_environments   TEXT    NOT NULL,
    requested_pull_requests  TEXT    NOT NULL,
    requested_metadata       TEXT    NOT NULL,
    granted_actions          TEXT    NOT NULL,
    granted_administration   TEXT    NOT NULL,
    granted_contents         TEXT    NOT NULL,
    granted_environments     TEXT    NOT NULL,
    granted_pull_requests    TEXT    NOT NULL,
    granted_metadata         TEXT    NOT NULL,
    expires_at               TEXT    CHECK (expires_at IS NULL OR expires_at <> '')
) STRICT;
