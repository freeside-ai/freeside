-- Automation trust profiles, workflow audits, and candidate authorizations
-- (plan §5.5, §5.6; issue #172). Like the mint audit (0006), all three are
-- daemon-internal trust bookkeeping, never synchronized state, so no
-- entity_version/as_of_revision columns. Timestamps are RFC3339Nano UTC
-- written by Go; the store never relies on SQLite clock functions. The
-- domain shapes are self-certifying (digest/id recomputed by Validate), so
-- the body carries the record and the extracted columns exist for keys,
-- lookups, and read-time cross-checks only.
--
-- trust_profiles is write-once per content digest: a revised profile is new
-- content under a new digest, never an update, and which digest is "current"
-- is a consumer decision (§5.5 binds runs and publication by digest).
-- recorded_at is bookkeeping for that selection, not part of the content.
-- The (repo, profile_digest) unique key exists solely as the composite
-- foreign-key target below: profile_digest alone is already the primary
-- key, but an FK on it alone would let an authorization for one repository
-- reference another repository's profile row.
CREATE TABLE trust_profiles (
    profile_digest TEXT NOT NULL PRIMARY KEY CHECK (profile_digest <> ''),
    repo           TEXT NOT NULL CHECK (repo <> ''),
    recorded_at    TEXT NOT NULL CHECK (recorded_at <> ''),
    body           TEXT NOT NULL,
    UNIQUE (repo, profile_digest)
) STRICT;

-- workflow_audits is an insert-only observation ledger: two identical audits
-- at different times are two real observations, so there is no idempotency
-- key and no dedup (the mint-audit shape).
CREATE TABLE workflow_audits (
    id                    INTEGER PRIMARY KEY,
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    audited_commit_sha    TEXT NOT NULL CHECK (audited_commit_sha <> ''),
    audited_at            TEXT NOT NULL CHECK (audited_at <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    body                  TEXT NOT NULL
) STRICT;

-- candidate_authorizations is write-once per content id. The composite
-- foreign key makes an authorization without its bound profile row
-- unrepresentable AND binds it to a profile recorded for the same
-- repository: an FK on the digest alone would accept another repository's
-- automation posture (fail closed; the store opens with foreign_keys on).
-- The per-profile uniqueness key is an owner decision: the same candidate
-- head may be re-authorized under a human-approved revised profile (§5.5
-- drift recovery), while a second authorization for one head under one
-- profile is a conflict.
CREATE TABLE candidate_authorizations (
    id                   TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha             TEXT NOT NULL CHECK (head_sha <> ''),
    trust_profile_digest TEXT NOT NULL CHECK (trust_profile_digest <> ''),
    created_at           TEXT NOT NULL CHECK (created_at <> ''),
    body                 TEXT NOT NULL,
    UNIQUE (repo, head_sha, trust_profile_digest),
    FOREIGN KEY (repo, trust_profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;
