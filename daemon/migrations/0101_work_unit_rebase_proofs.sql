-- Rebase proofs (issue #2129): the write-once verdict on whether a pull
-- request head someone else pushed is the reviewed candidate rebased onto the
-- base the last review used. A lander that requires up-to-date branches
-- rebases before it merges, so the merged head is never the reviewed one. A
-- proven row lets the completion evaluator accept a merge of that head; a
-- refused row records why a merge of it forecloses the unit instead.
--
-- Daemon-internal like the 0026 capture rows: never synchronized, and the
-- body is the authority. Readers re-gate a proven row against the latest
-- review record and the published head, so a decoded proven bit alone never
-- moves the head completion compares.
CREATE TABLE work_unit_rebase_proofs (
    unit_id         TEXT NOT NULL REFERENCES work_unit_pr_bindings (unit_id),
    proven_head_sha TEXT NOT NULL CHECK (proven_head_sha <> ''),
    body            TEXT NOT NULL CHECK (body <> ''),
    recorded_at     TEXT NOT NULL CHECK (recorded_at <> ''),
    PRIMARY KEY (unit_id, proven_head_sha)
) STRICT;
