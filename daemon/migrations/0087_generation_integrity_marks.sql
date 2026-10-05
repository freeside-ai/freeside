-- Credential-integrity marks (plan §5.4 admission rule 4, issue #1624): one
-- row per enrollment generation and finding class, written when the
-- credential-integrity probe finds that generation's stored credential
-- truncated or corrupted. A marked generation is not credentialed, so
-- admission refuses it; re-enrollment appends a new generation, which has no
-- row here, and that is how the refusal clears.
--
-- The mark is its own append-only table, not a column on
-- client_enrollment_generations: generation rows are immutable, and every
-- boundary that decides whether work may start reads these rows itself
-- instead of trusting a bit carried on another record. Rows are never
-- updated or deleted.
--
-- The primary key is the recording conflict target: a repeat of the same
-- finding for the same generation keeps the first row, so the scheduled
-- probe can record on every pass. The foreign key means a mark can only name
-- a generation that exists. The canonical JSON body is the authority; the
-- other columns are lookup keys that reconstruction cross-checks against the
-- decoded body.
--
-- Daemon-internal like the generations it names: never synchronized. The
-- table starts empty: nothing marked a generation before it.
CREATE TABLE generation_integrity_marks (
    enrollment_id TEXT    NOT NULL CHECK (enrollment_id <> ''),
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 1),
    finding       TEXT    NOT NULL CHECK (finding <> ''),
    observed_at   TEXT    NOT NULL CHECK (observed_at <> ''),
    body          TEXT    NOT NULL CHECK (body <> ''),
    PRIMARY KEY (enrollment_id, ordinal, finding),
    FOREIGN KEY (enrollment_id, ordinal)
        REFERENCES client_enrollment_generations (enrollment_id, ordinal)
) STRICT;
