-- Retain the canonical body addressed by a workflow audit digest separately
-- from the insert-only observation ledger (#274). Identical audits deduplicate
-- by (repo, digest), while store retention may delete bodies without rewriting
-- historical audit facts. The 16 MiB ceiling matches
-- domain.MaxWorkflowAuditEvidenceBytes.
CREATE TABLE workflow_audit_evidence (
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    body                  BLOB NOT NULL
                          CHECK (length(body) > 0 AND length(body) <= 16777216),
    PRIMARY KEY (repo, workflow_audit_digest)
) STRICT;

-- Bind the reviewed workflow digest into the activation row so current
-- activations do not need a current-version profile decode merely to carry
-- the coordinate. This copied value is only a hint: every pruning operation
-- re-authenticates it against the profile's content digest and current
-- validation. Existing stale or tampered profiles therefore retain extra
-- evidence rather than becoming deletion authority. Malformed bodies fail
-- the migration instead of selecting a digest.
DROP INDEX trust_profile_activations_repo_id;
ALTER TABLE trust_profile_activations
    RENAME TO trust_profile_activations_v1;

CREATE TABLE trust_profile_activations (
    id                    INTEGER PRIMARY KEY,
    repo                  TEXT NOT NULL CHECK (repo <> ''),
    profile_digest        TEXT NOT NULL CHECK (profile_digest <> ''),
    workflow_audit_digest TEXT NOT NULL CHECK (workflow_audit_digest <> ''),
    activated_at          TEXT NOT NULL CHECK (activated_at <> ''),
    FOREIGN KEY (repo, profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;

INSERT INTO trust_profile_activations (
    id, repo, profile_digest, workflow_audit_digest, activated_at
)
SELECT
    a.id,
    a.repo,
    a.profile_digest,
    CAST(json_extract(p.body, '$.workflow_audit_digest') AS TEXT),
    a.activated_at
FROM trust_profile_activations_v1 AS a
JOIN trust_profiles AS p
  ON p.repo = a.repo AND p.profile_digest = a.profile_digest;

DROP TABLE trust_profile_activations_v1;

CREATE INDEX trust_profile_activations_repo_id
    ON trust_profile_activations(repo, id);
