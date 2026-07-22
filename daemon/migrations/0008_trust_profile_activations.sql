-- Explicit current-profile selection (plan §5.5; issue #182). Profile rows
-- remain immutable content addressed by profile_digest. Activations are the
-- append-only owner decisions that select one of those rows as current, so an
-- exact prior profile can be re-approved after an intervening revision.
-- Selection by profile insertion order could not represent A -> B -> A.
CREATE TABLE trust_profile_activations (
    id             INTEGER PRIMARY KEY,
    repo           TEXT NOT NULL CHECK (repo <> ''),
    profile_digest TEXT NOT NULL CHECK (profile_digest <> ''),
    activated_at   TEXT NOT NULL CHECK (activated_at <> ''),
    FOREIGN KEY (repo, profile_digest) REFERENCES trust_profiles(repo, profile_digest)
) STRICT;

CREATE INDEX trust_profile_activations_repo_id
    ON trust_profile_activations(repo, id);

-- Preserve the pre-0008 meaning for an existing database: its newest profile
-- row becomes the initial explicit selection. A later encoding bump may make
-- that body stale; the current-profile read still validates it and fails
-- closed until an owner records or activates a current encoding.
INSERT INTO trust_profile_activations (repo, profile_digest, activated_at)
SELECT tp.repo, tp.profile_digest, tp.recorded_at
FROM trust_profiles AS tp
WHERE tp.rowid = (
    SELECT MAX(newest.rowid)
    FROM trust_profiles AS newest
    WHERE newest.repo = tp.repo
);
