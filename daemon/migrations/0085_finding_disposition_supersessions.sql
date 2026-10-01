-- One immutable supersession record per reversed fix (plan §7 Review Drift,
-- issue #1666): a drift audit's reversal undid a `fixed` disposition, so the
-- finding's effective latest disposition reads as declined from the reversing
-- round on. The canonical JSON body is the authority; finding_id,
-- superseded_round, run_id, and reversing_round are lookup keys that
-- reconstruction cross-checks against the decoded body before any caller acts
-- on the row.
--
-- (finding_id, superseded_round) is the superseded disposition's key. It is
-- the PRIMARY KEY and the putImmutable conflict target, so a disposition has
-- at most one record: a byte-identical replay converges and a different
-- record for the same disposition is an immutable conflict. The composite
-- foreign keys bind the disposition's and the reversing round's existence.
-- That the disposition is `fixed`, that the named audit proposed the
-- reversal, and that the authority holds live in the store accessor, because
-- those are other rows' bodies.
--
-- A disposition with no row here is not superseded. Rows are written while
-- the reversing round is the run's latest and never backfilled, so no
-- pre-existing run gains a record here.
CREATE TABLE finding_disposition_supersessions (
    finding_id       TEXT    NOT NULL,
    superseded_round INTEGER NOT NULL CHECK (superseded_round > 0),
    run_id           TEXT    NOT NULL REFERENCES runs (id),
    reversing_round  INTEGER NOT NULL,
    body_digest      TEXT    NOT NULL CHECK (body_digest <> ''),
    body             TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (finding_id, superseded_round),
    FOREIGN KEY (finding_id, superseded_round) REFERENCES finding_dispositions (finding_id, round),
    FOREIGN KEY (run_id, reversing_round) REFERENCES review_records (run_id, round),
    CHECK (reversing_round > superseded_round)
) STRICT;
