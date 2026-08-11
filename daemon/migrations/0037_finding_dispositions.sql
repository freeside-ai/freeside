-- Per-finding review dispositions are immutable round history (plan §7).
-- The review record remains the authority for which finding belonged to a
-- round; the trigger closes the direct-SQL gap around that three-way binding.
CREATE TABLE finding_dispositions (
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

CREATE TRIGGER finding_disposition_requires_round_finding
BEFORE INSERT ON finding_dispositions
WHEN NOT EXISTS (
    SELECT 1
    FROM review_records AS record
    JOIN review_record_findings AS finding
      ON finding.invocation_id = record.invocation_id
    JOIN findings AS raw_finding
      ON raw_finding.id = finding.finding_id
     AND raw_finding.run_id = record.run_id
    JOIN json_each(record.body, '$.finding_ids') AS body_finding
      ON body_finding.value = finding.finding_id
    WHERE record.run_id = NEW.run_id
      AND record.round = NEW.round
      AND finding.finding_id = NEW.finding_id
      AND json_extract(record.body, '$.run_id') = record.run_id
      AND json_extract(record.body, '$.round') = record.round
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
      AND json_extract(raw_finding.body, '$.id') = raw_finding.id
      AND json_extract(raw_finding.body, '$.run_id') = raw_finding.run_id
)
BEGIN
    SELECT RAISE(ABORT, 'finding does not belong to review round');
END;

CREATE INDEX finding_dispositions_by_run
    ON finding_dispositions (run_id, round, finding_id);
