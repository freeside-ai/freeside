-- Dispositions for admitted external findings (plan §5.19, §7; issue #1749):
-- the durable outcome (declined, deferred, fixed) of a finding an admitted
-- external reviewer left on a published pull request.
--
-- The table is separate from finding_dispositions on purpose. Every reader of
-- that table takes a row as proof that its finding came from the round's
-- review record, and the signet review facts, the publication disposition
-- history, and the review yield history fail closed when it did not. An
-- external finding never joins a review record (the §5.19 quarantine), so its
-- outcome lives here, where review completeness, convergence, the drift
-- audit, and the publication history never read it.
--
-- The trigger closes the direct-SQL gap around what a row can name. It
-- proves the finding is an external one of the run, that a dispatched
-- external_review authority for the run starts its cycle at the row's round
-- on the finding's head, that the round's review record is on that
-- authority's base and head, and, for fixed, that the remediation is a later
-- review record of the run on the same base and another head. It does not
-- prove the reviewer is admitted or that an adjudication authorizes a
-- declined or deferred outcome: both are re-run in Go on every write and
-- read, as adjudication authority is for finding_dispositions.
CREATE TABLE external_finding_dispositions (
    finding_id  TEXT NOT NULL REFERENCES findings (id),
    run_id      TEXT NOT NULL REFERENCES runs (id),
    round       INTEGER NOT NULL CHECK (round > 0),
    disposition TEXT NOT NULL CHECK (disposition IN ('fixed', 'declined', 'deferred')),
    reason      TEXT NOT NULL CHECK (reason <> ''),
    remediation_invocation_id TEXT NOT NULL,
    created_at  TEXT NOT NULL CHECK (created_at <> ''),
    body_digest TEXT NOT NULL CHECK (body_digest <> ''),
    body        TEXT NOT NULL CHECK (body <> ''),
    PRIMARY KEY (finding_id, round),
    FOREIGN KEY (run_id, round) REFERENCES review_records (run_id, round),
    CHECK (
        (disposition = 'fixed' AND remediation_invocation_id <> '')
        OR
        (disposition <> 'fixed' AND remediation_invocation_id = '')
    )
) STRICT;

-- outbox.payload is a BLOB, which the JSON functions would read as JSONB, so
-- every read casts it to TEXT. The CASE fixes the evaluation order: a payload
-- that is not JSON is skipped before json_extract can raise on it.
CREATE TRIGGER external_finding_disposition_requires_admitted_cycle
BEFORE INSERT ON external_finding_dispositions
WHEN NOT EXISTS (
    SELECT 1
    FROM findings AS raw_finding
    JOIN review_records AS record
      ON record.run_id = raw_finding.run_id
     AND record.round = NEW.round
    WHERE raw_finding.id = NEW.finding_id
      AND raw_finding.run_id = NEW.run_id
      AND json_extract(raw_finding.body, '$.id') = raw_finding.id
      AND json_extract(raw_finding.body, '$.run_id') = raw_finding.run_id
      AND json_type(raw_finding.body, '$.external') = 'object'
      AND json_extract(record.body, '$.run_id') = record.run_id
      AND json_extract(record.body, '$.round') = record.round
      AND json_extract(record.body, '$.base_sha') = record.base_sha
      AND json_extract(record.body, '$.head_sha') = record.head_sha
      AND record.head_sha = json_extract(raw_finding.body, '$.external.head_sha')
      AND EXISTS (
          SELECT 1
          FROM outbox AS authority
          WHERE authority.kind = 'publication_successor_authority'
            AND authority.status = 'dispatched'
            AND CASE WHEN json_valid(CAST(authority.payload AS TEXT)) THEN
                    json_extract(CAST(authority.payload AS TEXT), '$.run_id') = record.run_id
                    AND json_extract(CAST(authority.payload AS TEXT), '$.origin') = 'external_review'
                    AND json_extract(CAST(authority.payload AS TEXT), '$.review_round') = record.round
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.base_sha') = record.base_sha
                    AND json_extract(CAST(authority.payload AS TEXT), '$.reentry.head_sha') = record.head_sha
                ELSE 0 END
      )
      AND (NEW.disposition <> 'fixed' OR EXISTS (
          SELECT 1
          FROM review_records AS remediation
          WHERE remediation.invocation_id = NEW.remediation_invocation_id
            AND remediation.run_id = record.run_id
            AND remediation.round > record.round
            AND remediation.base_sha = record.base_sha
            AND remediation.head_sha <> record.head_sha
            AND json_extract(remediation.body, '$.run_id') = remediation.run_id
            AND json_extract(remediation.body, '$.round') = remediation.round
            AND json_extract(remediation.body, '$.base_sha') = remediation.base_sha
            AND json_extract(remediation.body, '$.head_sha') = remediation.head_sha
      ))
)
BEGIN
    SELECT RAISE(ABORT, 'external finding does not belong to an external review cycle round');
END;

CREATE INDEX external_finding_dispositions_by_run
    ON external_finding_dispositions (run_id, round, finding_id);
