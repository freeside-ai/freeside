-- Per-round diff metrics for the review drift floor (plan §7 Review Drift,
-- issue #1048). One immutable row per review round holds the round's two named
-- pairs: cumulative (bound base against the round's candidate head) and round
-- (the previous round's candidate head against this round's). The canonical
-- JSON body is the authority; run_id and round are lookup keys that
-- reconstruction cross-checks against the decoded body.
--
-- (run_id, round) is the PRIMARY KEY and the putImmutable conflict target, so
-- a byte-identical replay converges and different metrics for the same round
-- are an immutable conflict. The composite foreign key binds the round's
-- existence. Agreement of the named commits with the review records lives in
-- the store accessor, because the previous round's head is another row's body.
--
-- A round with no row is a gap: rows are written when the round is recorded
-- and never backfilled, so no pre-existing run gains metrics here.
CREATE TABLE review_round_diff_metrics (
    run_id      TEXT    NOT NULL REFERENCES runs (id),
    round       INTEGER NOT NULL CHECK (round > 0),
    body_digest TEXT    NOT NULL CHECK (body_digest <> ''),
    body        TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (run_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round)
) STRICT;
