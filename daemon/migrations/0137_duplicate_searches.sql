-- One immutable, digest-addressed DuplicateSearch record per finding a
-- finding adjudication routes to a follow-up filing (plan §7 Finding
-- Adjudication, issue #3146). The canonical JSON body is the authority;
-- run_id, adjudication_digest, finding_id, and content_digest are lookup keys
-- that reconstruction cross-checks against the decoded body before any
-- caller acts on the row.
--
-- (adjudication_digest, finding_id) is the PRIMARY KEY and the putImmutable
-- conflict target, so a byte-identical replay converges and a different
-- search for the same entry is an immutable conflict: a finding gets one
-- search per adjudication artifact, and a revised adjudication searches
-- again under its own digest. content_digest is the record's content address
-- (a plain indexed column, not a competing unique index). Agreement of the
-- adjudication, the run, and the finding's route lives in the store
-- accessor, because those are other rows' bodies.
--
-- A finding with no row was never searched, and the follow-up filing gate
-- refuses to admit or approve a filing for it. Nothing is backfilled.
CREATE TABLE duplicate_searches (
    run_id              TEXT NOT NULL REFERENCES runs (id),
    adjudication_digest TEXT NOT NULL CHECK (adjudication_digest <> ''),
    finding_id          TEXT NOT NULL CHECK (finding_id <> ''),
    content_digest      TEXT NOT NULL CHECK (content_digest <> ''),
    body_digest         TEXT NOT NULL CHECK (body_digest <> ''),
    body                TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (adjudication_digest, finding_id)
) STRICT;

CREATE INDEX duplicate_searches_by_digest
    ON duplicate_searches (content_digest);
