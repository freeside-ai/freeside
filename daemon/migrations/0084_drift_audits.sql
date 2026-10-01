-- One immutable, digest-addressed DriftAudit artifact per audited review round
-- (plan §7 Review Drift, issue #1665). The canonical JSON body is the
-- authority; run_id, round, and content_digest are lookup keys that
-- reconstruction cross-checks against the decoded body before any caller acts
-- on the row.
--
-- (run_id, round) is the PRIMARY KEY and the putImmutable conflict target, so
-- a byte-identical replay converges and a different audit for the same round
-- is an immutable conflict. An audit has no revision chain. content_digest is
-- the artifact's content address (a plain indexed column, not a competing
-- unique index, so putImmutable keeps a single conflict target). The composite
-- foreign key binds the round's existence. Agreement of the base, head,
-- specification, policy, and reversal findings with the run's records lives in
-- the store accessor, because those are other rows' bodies.
--
-- A round with no row has no verdict: the audit is off, did not run, or
-- failed. Rows are written while the round is the run's latest and never
-- backfilled, so no pre-existing run gains an audit here.
CREATE TABLE drift_audits (
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    round          INTEGER NOT NULL CHECK (round > 0),
    content_digest TEXT    NOT NULL CHECK (content_digest <> ''),
    body_digest    TEXT    NOT NULL CHECK (body_digest <> ''),
    body           TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (run_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;

CREATE INDEX drift_audits_by_digest
    ON drift_audits (content_digest);
