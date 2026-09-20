-- One immutable, digest-addressed PublicationAuthoring artifact: the advisory,
-- producer-labeled prose the explain site writes for a publication (plan §5.13,
-- §9). The canonical JSON body is the authority; the extracted columns are
-- store-private lookup and integrity keys that reconstruction cross-checks
-- against the decoded body before any caller acts on the row.
--
-- content_digest is the artifact's content address and the PRIMARY KEY, so a
-- byte-identical replay converges and a differing body under the same digest is
-- an immutable conflict. run_id is a plain foreign key into runs (id): a run may
-- have more than one authoring artifact (#1419 decides whether a base advance
-- reruns the author), so it is deliberately not unique. There is no
-- entity_version or as_of_revision column, because this is not a synced entity;
-- it lives in the store rather than the advisory store precisely so it is never
-- pruned while a publication PR depends on rendering the same bytes.
--
-- The evidence references, and their publish-eligibility and sensitivity gate,
-- live inside the JSON body a trigger cannot iterate, so the store accessor
-- enforces them. The foreign key binds the run's existence.
CREATE TABLE publication_authorings (
    content_digest TEXT NOT NULL CHECK (content_digest <> ''),
    run_id         TEXT NOT NULL REFERENCES runs (id),
    created_at     TEXT NOT NULL,
    body_digest    TEXT NOT NULL,
    body           TEXT NOT NULL,
    PRIMARY KEY (content_digest)
) STRICT;

-- The list-by-run read returns a run's artifacts oldest first, tie-broken by
-- digest; this index serves that ordering without a table scan.
CREATE INDEX publication_authorings_by_run
    ON publication_authorings (run_id, created_at, content_digest);
