-- Admitted agents (plan §5.4, issue #894): client enrollments, their
-- append-only store generations, and the narrowed identity declaration.
-- Daemon-internal records like the identities themselves: never synchronized,
-- so no entity_version/as_of_revision.
--
-- auth_identities gains the account and operator fields the dissolved
-- ProviderProfile left behind (account_binding, usage_pool, budget, enabled,
-- cost_owner). The client facts (auth_store_volume, refresh_strategy,
-- supports_read_only_auth_snapshot) move to the enrollment and its
-- generations in the domain contract; their columns remain here as the
-- cross-checked carrier of the identity's interim facts, because a
-- pre-enrollment identity has exactly one implicit client and the singular
-- locator is still truthful for it. Existing bodies are rewritten into the
-- narrowed shape with the facts under $.identity.interim; existing
-- identities are enabled (they are the live interim path), and their
-- account/pool/owner fields start empty until adoption binds them.
--
-- The 0013 CHECK (refresh_strategy <> '') cannot be dropped without a table
-- rebuild, which the migration harness's always-on foreign keys forbid, so a
-- post-adoption identity (no interim facts) writes the marker 'none' — a
-- value deliberately outside the RefreshStrategy enum — and the store's
-- reconstruction cross-check compares through the same mapping.
--
-- Nothing else is backfilled: no enrollment existed before this migration,
-- and inventing one would fabricate a store history nobody recorded
-- (adoption under #867 is the operator act that creates them).

ALTER TABLE auth_identities ADD COLUMN account_binding TEXT NOT NULL DEFAULT '';
ALTER TABLE auth_identities ADD COLUMN usage_pool TEXT NOT NULL DEFAULT '';
ALTER TABLE auth_identities ADD COLUMN budget INTEGER NOT NULL DEFAULT 0 CHECK (budget >= 0);
ALTER TABLE auth_identities ADD COLUMN enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1));
ALTER TABLE auth_identities ADD COLUMN cost_owner TEXT NOT NULL DEFAULT '';

UPDATE auth_identities SET enabled = 1;

UPDATE auth_identities SET body = json_set(
    json_remove(body,
        '$.identity.auth_store_volume',
        '$.identity.refresh_strategy',
        '$.identity.supports_read_only_auth_snapshot'),
    '$.identity.account_binding', '',
    '$.identity.usage_pool', '',
    '$.identity.budget', 0,
    '$.identity.enabled', json('true'),
    '$.identity.cost_owner', '',
    '$.identity.interim', json_object(
        'auth_store_volume',
            COALESCE(json_extract(body, '$.identity.auth_store_volume'), ''),
        'refresh_strategy',
            COALESCE(json_extract(body, '$.identity.refresh_strategy'), ''),
        'supports_read_only_auth_snapshot',
            CASE WHEN json_extract(body, '$.identity.supports_read_only_auth_snapshot')
                THEN json('true') ELSE json('false') END));

-- One subscription never holds two leases or two budgets (§5.4, the kept
-- revision 36 rule): an account binding, once set, belongs to one identity.
-- The store's write path refuses with a typed error first; this index is the
-- mechanical backstop.
CREATE UNIQUE INDEX auth_identities_account_binding
    ON auth_identities (account_binding) WHERE account_binding <> '';

-- One AuthIdentity × harness client × route × auth method (§5.4). The
-- quadruple is UNIQUE so a retried enrollment of the same client converges on
-- its row instead of minting a sibling. Enum membership is the domain's
-- valid() on every decode; CHECKs stay at non-emptiness.
CREATE TABLE client_enrollments (
    id               TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    auth_identity_id TEXT NOT NULL REFERENCES auth_identities (id),
    harness_client   TEXT NOT NULL CHECK (harness_client <> ''),
    route            TEXT NOT NULL CHECK (route <> ''),
    auth_method      TEXT NOT NULL CHECK (auth_method <> ''),
    credential_mode  TEXT NOT NULL CHECK (credential_mode <> ''),
    refresh_strategy TEXT NOT NULL CHECK (refresh_strategy <> ''),
    supports_read_only_auth_snapshot INTEGER NOT NULL
        CHECK (supports_read_only_auth_snapshot IN (0, 1)),
    account_binding  TEXT NOT NULL CHECK (account_binding <> ''),
    recorded_at      TEXT NOT NULL CHECK (recorded_at <> ''),
    body             TEXT NOT NULL,
    UNIQUE (auth_identity_id, harness_client, route, auth_method)
) STRICT;

-- Append-only store history: every successful store mutation appends one
-- entry (§5.4). The (enrollment, ordinal) key plus the store's contiguity
-- check at the write boundary make the history gap-free; rows are immutable
-- and never deleted. token_expiry is NULL exactly where the enrollment's
-- auth method observes no expiry (the Claude setup token).
CREATE TABLE client_enrollment_generations (
    enrollment_id         TEXT    NOT NULL REFERENCES client_enrollments (id),
    ordinal               INTEGER NOT NULL CHECK (ordinal >= 1),
    auth_store_volume     TEXT    NOT NULL CHECK (auth_store_volume <> ''),
    store_manifest_digest TEXT    NOT NULL CHECK (store_manifest_digest <> ''),
    lease_fence           INTEGER NOT NULL CHECK (lease_fence >= 1),
    account_binding       TEXT    NOT NULL CHECK (account_binding <> ''),
    token_expiry          TEXT,
    recorded_at           TEXT    NOT NULL CHECK (recorded_at <> ''),
    body                  TEXT    NOT NULL,
    PRIMARY KEY (enrollment_id, ordinal)
) STRICT;

-- Adapter conformance (§5.4 admission step 3): what one adapter build's
-- stage contract suite proved, in the closed launch-capability vocabulary.
-- The BackendConformance posture exactly: append-only, the newest row per
-- adapter digest is the current declaration, id order decides supersession,
-- and enum membership is the domain's check on every decode. Runner
-- conformance (backend_conformance_records) is untouched.
CREATE TABLE adapter_conformance_records (
    id             INTEGER PRIMARY KEY,
    adapter_digest TEXT NOT NULL CHECK (adapter_digest <> ''),
    outcome        TEXT NOT NULL CHECK (outcome <> ''),
    -- Canonical LaunchCapabilitySet JSON; the literal 'null' exactly when
    -- the pass did not pass (a failed pass proves nothing).
    proved_capabilities TEXT NOT NULL CHECK (proved_capabilities <> ''),
    proved_at      TEXT NOT NULL CHECK (proved_at <> '')
) STRICT;
CREATE INDEX adapter_conformance_by_adapter
    ON adapter_conformance_records (adapter_digest, id);
-- No backfill: no adapter build durably proved anything before this
-- contract existed. Absence reads as unproved, and agent admission fails
-- closed on it.

-- The v4 admission encoding (§5.4 admission step 5) rides in the body; the
-- three columns below are the cross-checked, foreign-key-enforced keys an
-- agent-admitted row binds. All are NULL on every legacy admission — the
-- permanent legacy rule keeps a pre-cutover admission's identity and
-- credential mode without ever resolving it against current configuration —
-- and nothing is backfilled (the 0014 convention: inventing an agent for an
-- attempt that was not admitted through one would forge an audit fact).
ALTER TABLE execution_admissions ADD COLUMN agent_digest TEXT;
ALTER TABLE execution_admissions ADD COLUMN enrollment_id TEXT REFERENCES client_enrollments (id);
ALTER TABLE execution_admissions ADD COLUMN enrollment_generation INTEGER;
