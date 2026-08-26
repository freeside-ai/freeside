-- Evolve the one-artifact-per-round FindingAdjudication store into an
-- append-only revision chain. Revision 1 deliberately retains the existing
-- canonical body and content digest byte-for-byte; the new relational columns
-- make its implicit initial position explicit without rewriting authority.
CREATE TABLE finding_adjudications_revisions (
    run_id                      TEXT    NOT NULL REFERENCES runs (id),
    round                       INTEGER NOT NULL CHECK (round > 0),
    revision                    INTEGER NOT NULL CHECK (revision > 0),
    predecessor_digest          TEXT,
    content_digest              TEXT    NOT NULL CHECK (content_digest <> ''),
    finding_batch_digest        TEXT    NOT NULL CHECK (finding_batch_digest <> ''),
    approved_spec_digest        TEXT    NOT NULL CHECK (approved_spec_digest <> ''),
    instruction_snapshot_digest TEXT    NOT NULL CHECK (instruction_snapshot_digest <> ''),
    resolved_policy_digest      TEXT    NOT NULL CHECK (resolved_policy_digest <> ''),
    created_at                  TEXT    NOT NULL,
    body_digest                 TEXT    NOT NULL,
    body                        TEXT    NOT NULL,
    CHECK ((revision = 1) = (predecessor_digest IS NULL)),
    PRIMARY KEY (run_id, round, revision),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;

INSERT INTO finding_adjudications_revisions
    (run_id, round, revision, predecessor_digest, content_digest,
     finding_batch_digest, approved_spec_digest, instruction_snapshot_digest,
     resolved_policy_digest, created_at, body_digest, body)
SELECT
    run_id, round, 1, NULL, content_digest,
    finding_batch_digest, approved_spec_digest, instruction_snapshot_digest,
    resolved_policy_digest, created_at, body_digest, body
FROM finding_adjudications;

DROP TABLE finding_adjudications;
ALTER TABLE finding_adjudications_revisions RENAME TO finding_adjudications;

-- content_digest is the semantic address, not a second conflict target. The
-- primary key remains the sole immutable-insert arbitration key.
CREATE INDEX finding_adjudications_by_digest
    ON finding_adjudications (content_digest);
