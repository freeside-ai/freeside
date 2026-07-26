-- Provider identities and their auth-store mutation leases (plan §5.4).
-- Daemon-internal bookkeeping like trust_profiles and mint audits: never
-- synchronized, so no entity_version/as_of_revision.
--
-- The lease table is the serialization point itself, one row per identity, so
-- "at most one holder" is the primary key rather than a rule every writer has
-- to remember. Expiry is stored as text for audit and as an integer for
-- comparison: RFC3339Nano trims trailing zeros, so the text column does not
-- order lexicographically and must never be compared as a bound.
--
-- Nothing is backfilled: no identity existed before this migration, and
-- inventing one would fabricate a declaration nobody made.

-- Every declared field is extracted, not left to the body alone: the identity
-- carries no content address, so a column is what makes a partially edited row
-- fail its cross-check rather than reading back as a larger parallelism limit
-- than anyone measured.
CREATE TABLE auth_identities (
    id                        TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    provider                  TEXT NOT NULL CHECK (provider <> ''),
    auth_store_mutation_lease INTEGER NOT NULL CHECK (auth_store_mutation_lease IN (0, 1)),
    max_parallel_executions   INTEGER NOT NULL CHECK (max_parallel_executions >= 1),
    refresh_strategy          TEXT    NOT NULL CHECK (refresh_strategy <> ''),
    supports_read_only_auth_snapshot INTEGER NOT NULL
        CHECK (supports_read_only_auth_snapshot IN (0, 1)),
    recorded_at               TEXT NOT NULL CHECK (recorded_at <> ''),
    body                      TEXT NOT NULL
) STRICT;

CREATE TABLE auth_store_mutation_leases (
    auth_identity_id     TEXT    NOT NULL PRIMARY KEY REFERENCES auth_identities (id),
    holder               TEXT    NOT NULL CHECK (holder <> ''),
    fence                INTEGER NOT NULL CHECK (fence > 0),
    acquired_at          TEXT    NOT NULL CHECK (acquired_at <> ''),
    expires_at           TEXT    NOT NULL CHECK (expires_at <> ''),
    expires_at_unix_nano INTEGER NOT NULL,
    released_at          TEXT,
    body                 TEXT    NOT NULL
) STRICT;
