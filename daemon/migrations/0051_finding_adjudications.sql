-- One immutable, digest-addressed FindingAdjudication artifact per review round
-- with findings (plan §7 Finding Adjudication). The canonical JSON body is the
-- authority; the extracted columns are store-private lookup and integrity keys
-- that reconstruction cross-checks against the decoded body before any caller
-- acts on the row.
--
-- The natural one-per-round key is (run_id, round): it is the PRIMARY KEY and
-- the putImmutable conflict target, so a byte-identical replay converges and a
-- differing artifact for the same round is an immutable conflict. content_digest
-- is the artifact's semantic content address (a plain indexed column, not a
-- competing unique index, so putImmutable keeps a single conflict target). The
-- finding-batch equality to the review round's finding set is enforced by the
-- store accessor, whose entries live inside the JSON body a trigger cannot
-- iterate; the composite foreign key binds the round's existence.
CREATE TABLE finding_adjudications (
    run_id                      TEXT    NOT NULL REFERENCES runs (id),
    round                       INTEGER NOT NULL CHECK (round > 0),
    content_digest              TEXT    NOT NULL CHECK (content_digest <> ''),
    finding_batch_digest        TEXT    NOT NULL CHECK (finding_batch_digest <> ''),
    approved_spec_digest        TEXT    NOT NULL CHECK (approved_spec_digest <> ''),
    instruction_snapshot_digest TEXT    NOT NULL CHECK (instruction_snapshot_digest <> ''),
    resolved_policy_digest      TEXT    NOT NULL CHECK (resolved_policy_digest <> ''),
    created_at                  TEXT    NOT NULL,
    body_digest                 TEXT    NOT NULL,
    body                        TEXT    NOT NULL,
    PRIMARY KEY (run_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;

CREATE INDEX finding_adjudications_by_digest
    ON finding_adjudications (content_digest);
