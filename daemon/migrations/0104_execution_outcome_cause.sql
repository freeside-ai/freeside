-- The failure cause of a failed execution outcome (issue #899). The column
-- mirrors the body's cause the way status and summary do, so the store can
-- cross-check it on read. A failed outcome recorded before causes existed has
-- no positive cause, so it takes unknown, which is also what its body
-- canonicalizes to on decode; every other status carries no cause.
ALTER TABLE execution_outcomes ADD COLUMN cause TEXT NOT NULL DEFAULT '';

UPDATE execution_outcomes SET cause = 'unknown' WHERE status = 'failed';
