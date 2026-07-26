-- Candidate authorization v2 binds the complete ordered evidence snapshot.
-- A v1 row cannot be upgraded safely because it did not retain that digest.
-- Preserve those rows as audit history while removing them from the active
-- uniqueness domain so a fresh verification can record a v2 authorization
-- for the same repository, head, and trust profile.
CREATE TABLE legacy_candidate_authorizations (
    id                   TEXT NOT NULL PRIMARY KEY CHECK (id <> ''),
    repo                 TEXT NOT NULL CHECK (repo <> ''),
    base_sha             TEXT NOT NULL CHECK (base_sha <> ''),
    head_sha             TEXT NOT NULL CHECK (head_sha <> ''),
    trust_profile_digest TEXT NOT NULL CHECK (trust_profile_digest <> ''),
    created_at           TEXT NOT NULL CHECK (created_at <> ''),
    body                 TEXT NOT NULL,
    retired_reason       TEXT NOT NULL CHECK (retired_reason <> ''),
    FOREIGN KEY (repo, trust_profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;

INSERT INTO legacy_candidate_authorizations (
    id, repo, base_sha, head_sha, trust_profile_digest, created_at, body,
    retired_reason
)
SELECT
    id, repo, base_sha, head_sha, trust_profile_digest, created_at, body,
    'v1 authorization does not bind evidence_snapshot_digest'
FROM candidate_authorizations;

-- A pending publication intent bound to an archived v1 authorization cannot
-- be retargeted to a new decision. Preserve it for audit, but remove it from
-- the active recovery scan so it cannot permanently block later intents.
UPDATE outbox
SET status = 'quarantined'
WHERE kind = 'publish.publication'
  AND status = 'pending'
  AND CASE
        WHEN json_valid(payload)
        THEN json_extract(payload, '$.authorization_id')
      END IN (SELECT id FROM legacy_candidate_authorizations);

DELETE FROM candidate_authorizations;
